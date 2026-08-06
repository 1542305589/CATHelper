"""extractor 单元测试：parse_token_id / save_original_params / inject_params /
extract_chat / extract_completions / strip_chat / strip_completions（spec §2.2 §2.3）。"""
from __future__ import annotations

import json

from _helpers import (
    build_chat_response,
    build_completions_response,
    chat_top_entry,
)
from vllm_anomaly_middleware.extractor import (
    OriginalParams,
    extract_chat_response,
    extract_completions_response,
    inject_params,
    parse_token_id,
    save_original_params,
    strip_chat_response,
    strip_completions_response,
)

NI = "你"  # E4 BD A0 = [228,189,160]
HAO = "好"  # E5 A5 BD = [229,165,189]


# --------------------------- parse_token_id --------------------------- #
def test_parse_token_id_formats():
    assert parse_token_id("token_id:1234") == 1234
    assert parse_token_id("1234") == 1234
    assert parse_token_id(1234) == 1234
    assert parse_token_id(" token_id:5678 ") == 5678
    assert parse_token_id("token_id:abc") == -1
    assert parse_token_id("xyz") == -1
    assert parse_token_id("") == -1
    assert parse_token_id(None) == -1
    assert parse_token_id(True) == -1  # bool 不当 int


# --------------------------- save_original_params --------------------------- #
def test_save_original_params_chat():
    body = {
        "model": "glm-4-7",
        "logprobs": True,
        "top_logprobs": 5,
        "return_tokens_as_token_ids": True,
        "n": 4,
        "stream": True,
    }
    orig = save_original_params(body, True)
    assert orig.is_chat is True
    assert orig.logprobs is True
    assert orig.top_logprobs == 5
    assert orig.return_tokens_as_token_ids is True
    assert orig.n == 4
    assert orig.stream is True


def test_save_original_params_completions_defaults():
    body = {"model": "glm-4-7", "logprobs": 3}
    orig = save_original_params(body, False)
    assert orig.is_chat is False
    assert orig.logprobs == 3
    assert orig.top_logprobs is None
    assert orig.return_tokens_as_token_ids is False
    assert orig.n == 1
    assert orig.stream is False


def test_save_original_params_no_logprobs():
    orig = save_original_params({"model": "m"}, True)
    assert orig.logprobs is None
    assert orig.top_logprobs is None
    assert orig.return_tokens_as_token_ids is False


# --------------------------- inject_params --------------------------- #
def test_inject_chat_no_logprobs():
    body = {"model": "m", "messages": []}
    out = inject_params(body, True, 20)
    parsed = json.loads(out)
    assert parsed["logprobs"] is True
    assert parsed["top_logprobs"] == 20
    assert parsed["return_tokens_as_token_ids"] is True
    # Content-Length 由 middleware patch；此处仅校验 body 长度可序列化
    assert len(out) == len(out)


def test_inject_chat_max_client_vs_n():
    # 客户端 top_logprobs=5, N=20 → 注入 20
    out = inject_params({"top_logprobs": 5}, True, 20)
    assert json.loads(out)["top_logprobs"] == 20
    # 客户端 top_logprobs=10, N=5 → 注入 10
    out = inject_params({"top_logprobs": 10}, True, 5)
    assert json.loads(out)["top_logprobs"] == 10


def test_inject_completions_max():
    # 客户端 logprobs=5, N=20 → 20
    out = inject_params({"logprobs": 5}, False, 20)
    p = json.loads(out)
    assert p["logprobs"] == 20
    assert p["return_tokens_as_token_ids"] is True
    # 客户端 logprobs=10, N=5 → 10
    out = inject_params({"logprobs": 10}, False, 5)
    assert json.loads(out)["logprobs"] == 10
    # 未带 → N
    out = inject_params({}, False, 7)
    assert json.loads(out)["logprobs"] == 7


# --------------------------- extract --------------------------- #
def test_extract_chat_truncates_to_n():
    e = chat_top_entry(100, NI, -0.1, n_top=20)
    data = build_chat_response("glm-4-7", [e])
    res = extract_chat_response(data, n_detect=4)
    assert len(res) == 1
    topk_list, tokens = res[0]
    assert tokens == [100]
    assert len(topk_list) == 1
    assert len(topk_list[0]) == 4  # 截断到 N=4
    # 按 logprob 降序，第一项最大
    vals = list(topk_list[0].values())
    assert vals == sorted(vals, reverse=True)


def test_extract_completions_truncates_to_n():
    data = build_completions_response("glm-4-7", [100, 200], [-0.1, -0.2], n_top=20)
    res = extract_completions_response(data, n_detect=4)
    assert len(res) == 1
    topk_list, tokens = res[0]
    assert tokens == [100, 200]
    assert len(topk_list) == 2
    assert all(len(d) == 4 for d in topk_list)


# --------------------------- strip chat --------------------------- #
def test_strip_chat_no_logprobs_becomes_null():
    e = chat_top_entry(100, NI, -0.1, n_top=20)
    data = build_chat_response("glm-4-7", [e])
    orig = OriginalParams(True, None, None, False, 1, False)  # 未请求 logprobs
    strip_chat_response(data, orig)
    assert data["choices"][0]["logprobs"] is None
    assert "token_id:" not in json.dumps(data, ensure_ascii=False)


def test_strip_chat_truncate_decode_text():
    e = chat_top_entry(100, NI, -0.1, n_top=20)
    data = build_chat_response("glm-4-7", [e])
    orig = OriginalParams(True, True, 3, False, 1, False)  # logprobs=true,top_logprobs=3
    strip_chat_response(data, orig)
    entry = data["choices"][0]["logprobs"]["content"][0]
    assert entry["token"] == NI  # 从 bytes 解码为文本，非 token_id:
    assert len(entry["top_logprobs"]) == 3
    for tp in entry["top_logprobs"]:
        assert tp["token"] == NI
    assert "token_id:" not in json.dumps(data, ensure_ascii=False)


def test_strip_chat_keep_token_ids_when_requested():
    e = chat_top_entry(100, NI, -0.1, n_top=20)
    data = build_chat_response("glm-4-7", [e])
    orig = OriginalParams(True, True, 3, True, 1, False)  # return_tokens_as_token_ids=True
    strip_chat_response(data, orig)
    entry = data["choices"][0]["logprobs"]["content"][0]
    assert entry["token"] == "token_id:100"  # 原样保留
    assert len(entry["top_logprobs"]) == 3
    assert entry["top_logprobs"][0]["token"].startswith("token_id:")


def test_strip_chat_detect_truncate_vs_client_truncate():
    # 客户端 logprobs=10, N=4：注入 10，每 token 10 项；检测截前 4，返回客户端 10
    e = chat_top_entry(100, NI, -0.1, n_top=10)
    data = build_chat_response("glm-4-7", [e])
    # 检测抽取截断到 N=4
    res = extract_chat_response(data, n_detect=4)
    assert len(res[0][0][0]) == 4
    # 客户端恢复截断到 10
    orig = OriginalParams(True, True, 10, False, 1, False)
    strip_chat_response(data, orig)
    entry = data["choices"][0]["logprobs"]["content"][0]
    assert len(entry["top_logprobs"]) == 10


def test_strip_chat_no_bytes_token_becomes_null():
    e = chat_top_entry(100, NI, -0.1, n_top=5)
    e["bytes"] = None  # 无 bytes
    for tp in e["top_logprobs"]:
        tp["bytes"] = None
    data = build_chat_response("glm-4-7", [e])
    orig = OriginalParams(True, True, 3, False, 1, False)
    strip_chat_response(data, orig)
    entry = data["choices"][0]["logprobs"]["content"][0]
    assert entry["token"] is None  # 无 bytes → null，绝不留 token_id:
    assert "token_id:" not in json.dumps(data, ensure_ascii=False)


def test_strip_chat_n_choices_loop():
    e = chat_top_entry(100, NI, -0.1, n_top=5)
    data = build_chat_response("glm-4-7", [e], n=4)
    orig = OriginalParams(True, None, None, False, 4, False)
    strip_chat_response(data, orig)
    for choice in data["choices"]:
        assert choice["logprobs"] is None
    assert "token_id:" not in json.dumps(data, ensure_ascii=False)


# --------------------------- strip completions --------------------------- #
def test_strip_completions_no_logprobs_null():
    data = build_completions_response("glm-4-7", [100], [-0.1], n_top=20)
    orig = OriginalParams(False, None, None, False, 1, False)
    strip_completions_response(data, orig)
    assert data["choices"][0]["logprobs"] is None
    assert "token_id:" not in json.dumps(data, ensure_ascii=False)


def test_strip_completions_token_ids_kept_truncated():
    data = build_completions_response("glm-4-7", [100, 200], [-0.1, -0.2], n_top=20)
    orig = OriginalParams(False, 3, None, True, 1, False)  # logprobs=3, token_ids=True
    strip_completions_response(data, orig)
    lp = data["choices"][0]["logprobs"]
    assert lp["tokens"] == ["token_id:100", "token_id:200"]  # 原样保留
    assert len(lp["top_logprobs"]) == 2
    for pos in lp["top_logprobs"]:
        assert len(pos) == 3  # 截断到 3
        assert all(k.startswith("token_id:") for k in pos.keys())


def test_strip_completions_no_token_ids_null():
    data = build_completions_response("glm-4-7", [100, 200], [-0.1, -0.2], n_top=20)
    orig = OriginalParams(False, 3, None, False, 1, False)  # logprobs=3, 无 token_ids
    strip_completions_response(data, orig)
    lp = data["choices"][0]["logprobs"]
    # 无 tokenizer → tokens 全 null，top_logprobs 全 null
    assert lp["tokens"] == [None, None]
    assert lp["top_logprobs"] == [None, None]
    assert "token_id:" not in json.dumps(data, ensure_ascii=False)
