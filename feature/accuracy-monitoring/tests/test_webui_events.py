"""events 单元测试：相邻快照增量合成、计数器回绕、多候选独立事件、基线语义。"""
from __future__ import annotations

from webui.events import AnomalyEvent, EventSynthesizer, Snapshot, ill_type_name


def test_ill_type_name_mapping():
    assert ill_type_name(1) == "rare_character"
    assert ill_type_name(2) == "garbled"
    assert ill_type_name(3) == "repetition"
    assert ill_type_name(4) == "nan_value"
    assert ill_type_name(99) == "unknown"


def test_first_snapshot_is_baseline_only():
    s = EventSynthesizer()
    snap = Snapshot(instance="a", ts=1.0, requests=5,
                    detected={(2, "m", "0"): 1})
    assert s.process(snap) is None
    assert s.process(snap) is not None  # 第二次才有增量


def test_delta_synthesis_counts():
    s = EventSynthesizer()
    prev = Snapshot(instance="a", ts=1.0, requests=10,
                    detected={(2, "m", "0"): 2})
    cur = Snapshot(instance="a", ts=2.0, requests=13,
                   detected={(2, "m", "0"): 5, (1, "m", "1"): 1})
    d = s.process(prev)
    assert d is None
    d = s.process(cur)
    assert d is not None
    assert d.requests == 3
    assert d.anomalies_total == 4
    assert d.by_type["garbled"] == 3
    assert d.by_type["rare_character"] == 1
    assert d.by_model["m"] == 4
    assert len(d.events) == 4


def test_multiple_choice_independent_events():
    s = EventSynthesizer()
    s.process(Snapshot(instance="a", ts=1.0, detected={(2, "m", "0"): 0, (2, "m", "1"): 0}))
    d = s.process(Snapshot(instance="a", ts=2.0, detected={(2, "m", "0"): 1, (2, "m", "1"): 1}))
    assert d is not None
    events = d.events
    assert len(events) == 2
    assert {e.choice_index for e in events} == {"0", "1"}
    assert all(e.ill_type == "garbled" for e in events)
    assert all(e.instance == "a" and e.model == "m" for e in events)
    assert all(e.id > 0 for e in events)


def test_counter_wrap_ignores_negative():
    """实例重启/回绕：值变小 → 忽略负增量，不产生事件。"""
    s = EventSynthesizer()
    s.process(Snapshot(instance="a", ts=1.0, requests=1000,
                       detected={(2, "m", "0"): 100}))
    d = s.process(Snapshot(instance="a", ts=2.0, requests=5,
                           detected={(2, "m", "0"): 3}))
    assert d is not None
    assert d.requests == 0
    assert d.anomalies_total == 0
    assert d.events == []


def test_same_series_multi_increment_creates_multiple_events():
    s = EventSynthesizer()
    s.process(Snapshot(instance="a", ts=1.0, detected={(3, "m", "0"): 0}))
    d = s.process(Snapshot(instance="a", ts=2.0, detected={(3, "m", "0"): 3}))
    assert d is not None
    assert len(d.events) == 3
    assert all(e.ill_type == "repetition" for e in d.events)


def test_reset_makes_next_snapshot_baseline():
    """暂停/恢复后 reset → 下一份快照仅作基线（跳过增量）。"""
    s = EventSynthesizer()
    s.process(Snapshot(instance="a", ts=1.0, detected={(2, "m", "0"): 1}))
    s.reset("a")
    d = s.process(Snapshot(instance="a", ts=2.0, detected={(2, "m", "0"): 100}))
    assert d is None  # 恢复后首快照仅基线
    d = s.process(Snapshot(instance="a", ts=3.0, detected={(2, "m", "0"): 101}))
    assert d is not None
    assert d.anomalies_total == 1


def test_event_id_allocator_external():
    s = EventSynthesizer()
    ids = iter([10, 20])
    s.set_id_source(lambda: next(ids))
    s.process(Snapshot(instance="a", ts=1.0, detected={(1, "m", "0"): 0}))
    d = s.process(Snapshot(instance="a", ts=2.0, detected={(1, "m", "0"): 2}))
    assert d is not None
    assert [e.id for e in d.events] == [10, 20]
    assert all(isinstance(e, AnomalyEvent) for e in d.events)


def test_clear_drops_all_baselines():
    s = EventSynthesizer()
    s.process(Snapshot(instance="a", ts=1.0, detected={(1, "m", "0"): 1}))
    s.clear()
    assert s.process(Snapshot(instance="a", ts=2.0, detected={(1, "m", "0"): 5})) is None