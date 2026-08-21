# E2E Test Report

- **Run ID**: {{ run_id }}
- **Tier**: {{ tier }}
- **Models**: {{ models | join(", ") }}
- **PR**: {% if pr_number %}#{{ pr_number }} @ {{ pr_sha[:7] }}{% else %}local{% endif %}
- **Start Time**: {{ start_time }}
- **Duration**: {{ duration }}
- **Total**: {{ total }} | **Passed**: {{ passed }} | **Failed**: {{ failed }} | **Skipped**: {{ skipped }}
- **Stop-on-failure**: run aborted after first failed test (see `failures.log`)

## Test Results

| Test | TC ID | Priority | Model | Status | Duration | Summary |
|---|---|---|---|---|---|---|
{% for r in results %}
| {{ r.funcname }} | {{ r.tc_id }} | {{ r.priority }} | {{ r.model }} | {{ r.status }} | {{ r.duration }}s | {{ r.fail_summary }} |
{% endfor %}

{% if failed > 0 %}
## Failure Diagnostics

> Full traceback + diagnostics saved to `failures.log` and `diagnostics/<test>/`.

{% for r in results if r.status == "failed" %}
### {{ r.funcname }} ({{ r.tc_id }})
- **Error**: {{ r.fail_summary }}
- **Diagnostics**: `diagnostics/{{ r.funcname }}/`
{% endfor %}
{% endif %}

{% if skipped > 0 %}
## Skipped Tests

{% for r in results if r.status == "skipped" %}
### {{ r.funcname }} ({{ r.tc_id }})
- **Reason**: {{ r.fail_summary }}
{% endfor %}
{% endif %}
