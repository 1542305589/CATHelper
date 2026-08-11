"""SSEStreamProcessor 单元测试：跨块重组 / [DONE] / keepalive / 累积 / 每块恢复（spec §2.4）。"""
from __future__ import annotations

import json

from _helpers import chat_stream_chunk, chat_top_entry, completions_stream_chunk
from anomaly_middleware.extractor import OriginalParams, SSEStreamProcessor

NI = "你"
HAO = "好"


def _sse_bytes(obj) -> bytes:
    return b"data: " + json.dumps(obj, ensure_ascii=False).encode("utf-8") + b"\n\n"


def test_done_passthrough():
    sse = SSEStreamProcessor(True, OriginalParams(True, None, None, False, 1, False), 20)
    out = sse.feed(b"data: [DONE]\n\n")
    assert out == b"data: [DONE]\n\n"


def test_keepalive_passthrough():
    sse = SSEStreamProcessor(True, OriginalParams(True, None, None, False, 1, False), 20)
    out = sse.feed(b": keep-alive\n\n")
    assert out == b": keep-alive\n\n"


def test_cross_chunk_reassembly():
    sse = SSEStreamProcessor(
        True, OriginalParams(True, True, 3, False, 1, False), 20
    )
    e = chat_top_entry(100, NI, -0.1, n_top=20)
    chunk = chat_stream_chunk("glm-4-7", e, delta_text=NI)
    raw = _sse_bytes(chunk)
    half = len(raw) // 2
    out1 = sse.feed(raw[:half])
    assert out1 == b""  # 半事件留缓冲
    out2 = sse.feed(raw[half:])
    assert out2 != b""
    # 客户端收到一条完整事件
    assert out2.count(b"data: ") == 1
    assert out2.endswith(b"\n\n")


def test_multi_chunk_streaming_accumulation_and_strip():
    orig = OriginalParams(True, None, None, False, 1, False)  # 客户端未请求 logprobs
    sse = SSEStreamProcessor(True, orig, 20)
    e1 = chat_top_entry(100, NI, -0.1, n_top=20)
    e2 = chat_top_entry(200, HAO, -0.2, n_top=20)
    c1 = chat_stream_chunk("glm-4-7", e1, delta_text=NI)
    c2 = chat_stream_chunk("glm-4-7", e2, delta_text=HAO)
    out1 = sse.feed(_sse_bytes(c1))
    out2 = sse.feed(_sse_bytes(c2))
    out3 = sse.feed(b"data: [DONE]\n\n")
    # 每块增量转发（每块一条 data）
    assert out1.count(b"data: ") == 1
    assert out2.count(b"data: ") == 1
    assert out3 == b"data: [DONE]\n\n"
    # 每块已恢复：客户端未请求 logprobs → logprobs=null，无 token_id:
    assert b'"logprobs": null' in out1
    assert b"token_id:" not in out1
    # 累积检测数据：2 个 token
    topk_all, tokens_all = sse.get_detection_data()
    assert len(tokens_all) == 1  # 一个 choice
    assert tokens_all[0] == [100, 200]
    assert len(topk_all[0]) == 2
    assert len(topk_all[0][0]) == 20  # 截断到 N=20


def test_streaming_truncate_to_client_m():
    orig = OriginalParams(True, True, 3, False, 1, False)  # top_logprobs=3
    sse = SSEStreamProcessor(True, orig, 20)
    e = chat_top_entry(100, NI, -0.1, n_top=20)
    out = sse.feed(_sse_bytes(chat_stream_chunk("glm-4-7", e, delta_text=NI)))
    # 恢复：top_logprobs 截断到 3，token 为解码文本
    parsed = json.loads(out[len(b"data: "):].split(b"\n")[0])
    entry = parsed["choices"][0]["logprobs"]["content"][0]
    assert entry["token"] == NI
    assert len(entry["top_logprobs"]) == 3


def test_streaming_detection_data_keeps_full_topk_not_client_m():
    # 回归：客户端只请求 top_logprobs=2，但检测需 N=20。
    # 检测数据（get_detection_data）不得被客户端 M 截断，token 必须保持 token_id。
    orig = OriginalParams(True, True, 2, False, 1, True)  # 客户端 logprobs=True, top_logprobs=2
    sse = SSEStreamProcessor(True, orig, 20)
    e1 = chat_top_entry(100, NI, -0.1, n_top=20)
    e2 = chat_top_entry(200, HAO, -0.2, n_top=20)
    out1 = sse.feed(_sse_bytes(chat_stream_chunk("glm-4-7", e1, delta_text=NI)))
    out2 = sse.feed(_sse_bytes(chat_stream_chunk("glm-4-7", e2, delta_text=HAO)))
    # 客户端可见仍按 M=2 恢复（token 解码为文本，top_logprobs 截到 2）
    parsed = json.loads(out1[len(b"data: "):].split(b"\n")[0])
    entry = parsed["choices"][0]["logprobs"]["content"][0]
    assert entry["token"] == NI
    assert len(entry["top_logprobs"]) == 2
    # 检测数据保持完整 N=20，token 保持 token_id
    topk_all, tokens_all = sse.get_detection_data()
    assert tokens_all[0] == [100, 200]
    assert len(topk_all[0]) == 2
    assert len(topk_all[0][0]) == 20
    assert len(topk_all[0][1]) == 20


def test_completions_streaming_n3_keeps_choice_separate():
    # 回归：n=3 流式，各 choice 独立成组（按 choice.index，而非 chunk 内位置）
    orig = OriginalParams(False, None, None, False, 3, True)
    sse = SSEStreamProcessor(False, orig, 20)
    chunks = [
        completions_stream_chunk("glm-4-7", 101, -0.1, index=0),
        completions_stream_chunk("glm-4-7", 201, -0.2, index=1),
        completions_stream_chunk("glm-4-7", 301, -0.3, index=2),
        completions_stream_chunk("glm-4-7", 102, -0.11, index=0),
        completions_stream_chunk("glm-4-7", 202, -0.21, index=1),
        completions_stream_chunk("glm-4-7", 302, -0.31, index=2),
    ]
    for c in chunks:
        sse.feed(_sse_bytes(c))
    sse.feed(b"data: [DONE]\n\n")
    topk_all, tokens_all = sse.get_detection_data()
    assert len(tokens_all) == 3
    assert tokens_all[0] == [101, 102]
    assert tokens_all[1] == [201, 202]
    assert tokens_all[2] == [301, 302]
    assert len(topk_all[0]) == 2 and len(topk_all[1]) == 2 and len(topk_all[2]) == 2


def test_chat_streaming_n3_keeps_choice_separate():
    # 回归：n=3 chat 流式，各 choice 独立成组（按 choice.index）
    orig = OriginalParams(True, None, None, False, 3, True)
    sse = SSEStreamProcessor(True, orig, 20)
    chunks = [
        chat_stream_chunk("glm-4-7", chat_top_entry(100, NI, -0.1), delta_text="a", index=0),
        chat_stream_chunk("glm-4-7", chat_top_entry(200, HAO, -0.2), delta_text="b", index=1),
        chat_stream_chunk("glm-4-7", chat_top_entry(300, "c", -0.3), delta_text="c", index=2),
        chat_stream_chunk("glm-4-7", chat_top_entry(400, "d", -0.11), delta_text="d", index=0),
    ]
    for c in chunks:
        sse.feed(_sse_bytes(c))
    sse.feed(b"data: [DONE]\n\n")
    topk_all, tokens_all = sse.get_detection_data()
    assert len(tokens_all) == 3
    assert tokens_all[0] == [100, 400]
    assert tokens_all[1] == [200]
    assert tokens_all[2] == [300]


def test_crlf_compat():
    sse = SSEStreamProcessor(
        True, OriginalParams(True, True, 3, False, 1, False), 20
    )
    e = chat_top_entry(100, NI, -0.1, n_top=20)
    raw = b"data: " + json.dumps(chat_stream_chunk("glm-4-7", e)).encode() + b"\r\n\r\n"
    out = sse.feed(raw)
    assert out != b""
    assert out.endswith(b"\n\n")


def test_completions_streaming_accumulation():
    orig = OriginalParams(False, None, None, False, 1, False)
    sse = SSEStreamProcessor(False, orig, 20)
    c1 = completions_stream_chunk("glm-4-7", 100, -0.1, n_top=20)
    c2 = completions_stream_chunk("glm-4-7", 200, -0.2, n_top=20)
    sse.feed(_sse_bytes(c1))
    sse.feed(_sse_bytes(c2))
    sse.feed(b"data: [DONE]\n\n")
    topk_all, tokens_all = sse.get_detection_data()
    assert tokens_all[0] == [100, 200]
    assert len(topk_all[0]) == 2
    assert len(topk_all[0][0]) == 20


def test_flush_tail_without_done():
    """流式无 [DONE] 即断：flush 排空残余，按已累积数据检测。"""
    sse = SSEStreamProcessor(
        True, OriginalParams(True, None, None, False, 1, False), 20
    )
    e = chat_top_entry(100, NI, -0.1, n_top=20)
    raw = _sse_bytes(chat_stream_chunk("glm-4-7", e, delta_text=NI))
    # 不含尾部 \n\n
    sse.feed(raw[:-2])
    out = sse.flush()
    assert out != b""  # flush 补 \n\n
    assert out.endswith(b"\n\n")
    topk_all, tokens_all = sse.get_detection_data()
    assert tokens_all[0] == [100]


def test_non_json_data_passthrough():
    sse = SSEStreamProcessor(
        True, OriginalParams(True, None, None, False, 1, False), 20
    )
    out = sse.feed(b"data: not-json\n\n")
    assert out == b"data: not-json\n\n"  # 非 JSON 原样透传


def test_multi_data_line_event_passthrough():
    """多 data: 行（payload 非法 JSON）-> 原样透传（spec §2.4 不改非结构化事件）。"""
    sse = SSEStreamProcessor(
        True, OriginalParams(True, None, None, False, 1, False), 20
    )
    raw = b"data: {\"choices\": []}\ndata: tail\n\n"
    out = sse.feed(raw)
    assert out == raw


def test_extra_event_fields_preserved():
    """事件含 event:/id: 等附加字段 -> 恢复重发时保留。"""
    orig = OriginalParams(True, None, None, False, 1, False)
    sse = SSEStreamProcessor(True, orig, 20)
    e = chat_top_entry(100, NI, -0.1, n_top=20)
    chunk = chat_stream_chunk("glm-4-7", e, delta_text=NI)
    raw = (b"id: 7\nevent: message\ndata: " +
           json.dumps(chunk, ensure_ascii=False).encode() + b"\n\n")
    out = sse.feed(raw)
    assert b"id: 7" in out and b"event: message" in out  # 附加字段保留
    assert b'"logprobs": null' in out  # 仍执行恢复
    assert b"token_id:" not in out


def test_event_split_across_many_chunks():
    """单条事件被拆成多个 body 块 -> 全部重组后才输出（不半截转发）。"""
    sse = SSEStreamProcessor(
        True, OriginalParams(True, True, 3, False, 1, False), 20
    )
    e = chat_top_entry(100, NI, -0.1, n_top=20)
    raw = _sse_bytes(chat_stream_chunk("glm-4-7", e, delta_text=NI))
    outs = []
    for i in range(len(raw) - 1):
        out = sse.feed(raw[i:i + 1])  # 逐字节喂入
        outs.append(out)
        assert out == b""  # 事件未完整前不输出
    final = sse.feed(raw[-1:])
    assert final.count(b"data: ") == 1  # 补齐后输出一条完整事件
    assert final.endswith(b"\n\n")
    assert b"token_id:" not in final  # 恢复生效
