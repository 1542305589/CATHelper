from __future__ import annotations

_DEFAULT_IGNORE = {"id", "created", "_id"}


def normalize_response(resp: dict) -> dict:
    out = dict(resp)
    out.pop("id", None)
    out.pop("created", None)
    out.pop("_id", None)
    return out


def responses_equal(a: dict, b: dict, ignore_fields: set = None) -> bool:
    ignore = ignore_fields if ignore_fields is not None else _DEFAULT_IGNORE
    return _strip(a, ignore) == _strip(b, ignore)


def streams_equal(a: list, b: list) -> bool:
    """流式透明性比较：事件边界无关，重建到 token/content 层（TC-001）。

    vLLM 流式 detokenizer 对同一生成内容会以不同数量的 SSE delta 事件批量输出
    （按字节缓冲/时序 flush），因此事件个数与 [DONE] 位置不可跨运行复现
    （实测同一 no-mw 服务的 v3 基线两次运行 51 vs 50 个事件，内容逐 token 相同）。
    流式透明性必须在重建后校验：拼接 delta.content / text、展平 logprobs，
    而非逐事件逐字节比对。
    """
    return _flatten_stream(a) == _flatten_stream(b)


def _flatten_stream(events: list) -> dict:
    """将流式事件序列重建为事件边界无关的确定性结构。

    chat：delta.content 拼接 + logprobs.content 展平；
    completions：text 拼接 + tokens/token_logprobs/top_logprobs 展平。
    顶层的 id/created/_id 忽略，其余稳定字段首值保留。
    """
    ignore = _DEFAULT_IGNORE
    choices: dict = {}
    meta: dict = {}
    done = False
    for e in events or []:
        if not isinstance(e, dict):
            continue
        if e.get("done"):
            done = True
            continue
        for k, v in e.items():
            if k in ignore or k == "choices":
                continue
            meta.setdefault(k, v)
        for ch in e.get("choices") or []:
            if not isinstance(ch, dict):
                continue
            idx = ch.get("index", 0)
            acc = choices.setdefault(
                idx,
                {
                    "index": idx,
                    "delta": {},
                    "logprobs": {"content": []},
                    "finish_reason": None,
                    "text": [],
                    "tokens": [],
                    "token_logprobs": [],
                    "top_logprobs": [],
                    "text_offset": None,
                },
            )
            d = ch.get("delta")
            if isinstance(d, dict):
                if d.get("role") is not None and not acc["delta"].get("role"):
                    acc["delta"]["role"] = d["role"]
                content = d.get("content")
                if content:
                    acc["delta"]["content"] = acc["delta"].get("content", "") + content
                for k, v in d.items():
                    if k in ("role", "content") or k in acc["delta"]:
                        continue
                    acc["delta"][k] = v
            text = ch.get("text")
            if text:
                acc["text"].append(text)
            lp = ch.get("logprobs")
            if isinstance(lp, dict):
                if isinstance(lp.get("content"), list):
                    acc["logprobs"]["content"].extend(lp["content"])
                if isinstance(lp.get("tokens"), list):
                    acc["tokens"].extend(lp["tokens"])
                if isinstance(lp.get("token_logprobs"), list):
                    acc["token_logprobs"].extend(lp["token_logprobs"])
                if isinstance(lp.get("top_logprobs"), list):
                    acc["top_logprobs"].extend(lp["top_logprobs"])
                if acc["text_offset"] is None and lp.get("text_offset") is not None:
                    acc["text_offset"] = lp["text_offset"]
            if ch.get("finish_reason") is not None:
                acc["finish_reason"] = ch["finish_reason"]

    out = dict(meta)
    out["done"] = done
    out["choices"] = []
    for idx in sorted(choices):
        acc = choices[idx]
        for key in ("tokens", "token_logprobs", "top_logprobs"):
            if acc[key]:
                acc["logprobs"][key] = acc[key]
        if acc["text_offset"] is not None:
            acc["logprobs"]["text_offset"] = acc["text_offset"]
        acc["text"] = "".join(acc["text"])
        for key in ("tokens", "token_logprobs", "top_logprobs", "text_offset"):
            acc.pop(key, None)
        if not acc["logprobs"]["content"] and not acc["logprobs"].get("tokens"):
            acc["logprobs"] = None
        out["choices"].append(acc)
    return out


def _strip(obj, ignore: set):
    if isinstance(obj, dict):
        return {k: _strip(v, ignore) for k, v in obj.items() if k not in ignore}
    if isinstance(obj, list):
        return [_strip(v, ignore) for v in obj]
    return obj


_MAX_DIFFS_IN_MSG = 10


def canonicalize(resp: dict) -> dict:
    """把 chat / completions、流式（已重建）/ 非流式响应统一成规范视图。

    chat: choice.message.content / choice.delta.content + choice.logprobs.content
    completions: choice.text + choice.logprobs.{tokens,token_logprobs,top_logprobs}
    logprobs 的浮点 logprob 仅保留供 diff 展示，不参与相等比较。
    """
    if not isinstance(resp, dict):
        return {"object": None, "model": None, "usage": {}, "choices": []}
    return {
        "object": resp.get("object"),
        "model": resp.get("model"),
        "usage": _canonical_usage(resp.get("usage")),
        "choices": [_canonical_choice(ch) for ch in resp.get("choices") or [] if isinstance(ch, dict)],
    }


def _canonical_usage(usage) -> dict:
    if not isinstance(usage, dict):
        return {"prompt_tokens": None, "completion_tokens": None}
    return {
        "prompt_tokens": usage.get("prompt_tokens"),
        "completion_tokens": usage.get("completion_tokens"),
    }


def _canonical_choice(ch: dict) -> dict:
    msg = ch.get("message") if isinstance(ch.get("message"), dict) else {}
    delta = ch.get("delta") if isinstance(ch.get("delta"), dict) else {}
    content = msg.get("content")
    if content is None:
        content = delta.get("content")
    if content is None:
        content = ch.get("text")
    if content is None:
        content = ""
    return {
        "index": ch.get("index", 0),
        "finish_reason": ch.get("finish_reason"),
        "content": content,
        "logprobs": _canonical_logprobs(ch.get("logprobs")),
    }


def _canonical_logprobs(lp) -> list | None:
    if not isinstance(lp, dict):
        return None
    content = lp.get("content")
    if isinstance(content, list):
        out = []
        for pos in content:
            if not isinstance(pos, dict):
                out.append({"token": None, "logprob": None, "top_logprobs": []})
                continue
            out.append({
                "token": pos.get("token"),
                "logprob": pos.get("logprob"),
                "top_logprobs": _canonical_top(pos.get("top_logprobs")),
            })
        return out or None
    tokens = lp.get("tokens")
    if isinstance(tokens, list):
        tlp = lp.get("token_logprobs") or []
        tops = lp.get("top_logprobs") or []
        out = []
        for i in range(len(tokens)):
            out.append({
                "token": tokens[i],
                "logprob": tlp[i] if i < len(tlp) else None,
                "top_logprobs": _canonical_top(tops[i] if i < len(tops) else None),
            })
        return out or None
    return None


def _canonical_top(top) -> list:
    if not isinstance(top, list):
        return []
    out = []
    for e in top:
        if isinstance(e, dict):
            out.append({"token": e.get("token"), "logprob": e.get("logprob")})
        else:
            out.append({"token": e, "logprob": None})
    return out


def compare_transparency(actual: dict, baseline: dict, *, tail_delta: int = 1, tail_char_budget: int = 32) -> list:
    """结构化透明性比较，返回差异列表（空 = 通过）。

    只比较请求决定 / 跨 run 稳定的结构量，以及 greedy 通常稳定的生成量
    （在公共前缀上严格比较、尾部按 tail_delta / tail_char_budget 容差）。
    logprobs 浮点数值不参与比较。
    """
    a = canonicalize(actual)
    b = canonicalize(baseline)
    diffs: list = []
    _cmp_shape(a, b, diffs)
    _cmp_usage(a, b, tail_delta, diffs)
    _cmp_choices(a, b, tail_char_budget, diffs)
    return diffs


def _diff(path: str, expected, actual, diffs: list) -> None:
    diffs.append({"path": path, "expected": expected, "actual": actual})


def _cmp_shape(a: dict, b: dict, diffs: list) -> None:
    if a["object"] != b["object"]:
        _diff("object", b["object"], a["object"], diffs)
    if a["model"] != b["model"]:
        _diff("model", b["model"], a["model"], diffs)
    if len(a["choices"]) != len(b["choices"]):
        _diff("choices.length", len(b["choices"]), len(a["choices"]), diffs)
    for i in range(min(len(a["choices"]), len(b["choices"]))):
        ca, cb = a["choices"][i], b["choices"][i]
        if ca["index"] != cb["index"]:
            _diff(f"choices[{i}].index", cb["index"], ca["index"], diffs)
        if ca["finish_reason"] != cb["finish_reason"]:
            _diff(f"choices[{i}].finish_reason", cb["finish_reason"], ca["finish_reason"], diffs)
        if (ca["logprobs"] is None) != (cb["logprobs"] is None):
            _diff(
                f"choices[{i}].logprobs.presence",
                "absent" if cb["logprobs"] is None else "present",
                "absent" if ca["logprobs"] is None else "present",
                diffs,
            )

            


def _cmp_usage(a: dict, b: dict, tail_delta: int, diffs: list) -> None:
    pa, pb = a["usage"]["prompt_tokens"], b["usage"]["prompt_tokens"]
    if pa is not None and pb is not None and pa != pb:
        _diff("usage.prompt_tokens", pb, pa, diffs)
    ca, cb = a["usage"]["completion_tokens"], b["usage"]["completion_tokens"]
    if ca is not None and cb is not None and abs(ca - cb) > tail_delta:
        _diff("usage.completion_tokens", f"{cb} (±{tail_delta})", ca, diffs)


def _cmp_choices(a: dict, b: dict, tail_char_budget: int, diffs: list) -> None:
    n = min(len(a["choices"]), len(b["choices"]))
    for i in range(n):
        ca, cb = a["choices"][i], b["choices"][i]
        _cmp_content(i, ca["content"], cb["content"], tail_char_budget, diffs)
        la, lb = ca["logprobs"], cb["logprobs"]
        if la is None or lb is None:
            continue
        for p in range(min(len(la), len(lb))):
            pa, pb = la[p], lb[p]
            if len(pa["top_logprobs"]) != len(pb["top_logprobs"]):
                _diff(
                    f"choices[{i}].logprobs[{p}].top_logprobs.length",
                    len(pb["top_logprobs"]), len(pa["top_logprobs"]), diffs,
                )
            if pa["token"] != pb["token"]:
                _diff(f"choices[{i}].logprobs[{p}].token", pb["token"], pa["token"], diffs)
            # sa = {t["token"] for t in pa["top_logprobs"]}
            # sb = {t["token"] for t in pb["top_logprobs"]}
            # if sa != sb:
            #     _diff(
            #         f"choices[{i}].logprobs[{p}].top_logprobs.set",
            #         sorted(sb, key=lambda x: str(x)),
            #         sorted(sa, key=lambda x: str(x)),
            #         diffs,
            #     )
            ta, tb = pa["top_logprobs"], pb["top_logprobs"]
            if ta and tb and ta[0]["token"] != tb[0]["token"]:
                _diff(
                    f"choices[{i}].logprobs[{p}].top_logprobs[0].token",
                    tb[0]["token"],
                    ta[0]["token"],
                    diffs,
                )


def _cmp_content(i: int, a_text: str, b_text: str, budget: int, diffs: list) -> None:
    if a_text == b_text:
        return
    cp = _common_prefix_len(a_text, b_text)
    if cp < max(len(a_text), len(b_text)) - budget:
        _diff(f"choices[{i}].content", b_text, a_text, diffs)


def _common_prefix_len(a: str, b: str) -> int:
    n = min(len(a), len(b))
    i = 0
    while i < n and a[i] == b[i]:
        i += 1
    return i


def assert_response_transparent(actual: dict, baseline: dict, *, note: str = "", **kw) -> None:
    diffs = compare_transparency(actual, baseline, **kw)
    if diffs:
        raise AssertionError((f"{note}: " if note else "") + _format_diffs(diffs))


def assert_stream_transparent(events: list, baseline_events: list, *, note: str = "", **kw) -> None:
    diffs = compare_transparency(_flatten_stream(events), _flatten_stream(baseline_events), **kw)
    if diffs:
        raise AssertionError((f"{note}: " if note else "") + _format_diffs(diffs))


def _format_diffs(diffs: list) -> str:
    total = len(diffs)
    lines = [f"transparency mismatch ({total} diff(s)):"]
    for d in diffs[:_MAX_DIFFS_IN_MSG]:
        lines.append(f"  - {d['path']}: expected={d['expected']!r} actual={d['actual']!r}")
    if total > _MAX_DIFFS_IN_MSG:
        lines.append(f"  ... ({total - _MAX_DIFFS_IN_MSG} more)")
    return "\n".join(lines)
