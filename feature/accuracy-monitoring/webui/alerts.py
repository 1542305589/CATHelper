"""告警规则引擎 + 多渠道分发（Webhook / 邮件）。

- 规则按 (instance, model, ill_type) 归一化滑动窗口事件队列评估；
  队列长度 ≥ threshold → 触发一次告警并重置该窗口队列（避免同一事件反复触发）。
- 告警进 store 环形缓冲 + 界面横幅（前端按 id 增量消费）。
- 分发 fire-and-forget，失败仅记日志不重试；同一规则同一窗口内置顶去重
  （窗口重置语义）。规则可精确到单实例 / 单模型，也可 `*` 全局。
- Webhook 地址自动识别渠道并转换消息格式：
  飞书 / Lark、钉钉、企业微信（WeChat Work）；其余 URL 走通用 JSON。
- 邮件：配置 `email` 段（SMTP）后，每个告警同时发送邮件。
"""
from __future__ import annotations

import asyncio
import logging
from collections import defaultdict, deque
from dataclasses import dataclass
from typing import Any, Awaitable, Callable, Deque, Dict, List, Optional

from .config import AlertRule, EmailConfig
from .events import AnomalyEvent
from .store import RingBuffer

logger = logging.getLogger("webui.alerts")

# webhook 发送器协议：async def send(url, payload) -> None
WebhookSender = Callable[[str, Dict[str, Any]], Awaitable[None]]

# 异常类型中文标签（告警文案 / 各渠道消息共用）
ILL_LABELS = {
    "rare_character": "生僻字",
    "garbled": "乱码",
    "repetition": "重复",
    "nan_value": "NaN 值",
    "unknown": "未知",
}

# 各渠道机器人 Webhook 前缀；命中后自动转换为对应渠道的消息格式
FEISHU_WEBHOOK_PREFIXES = (
    "https://open.feishu.cn/open-apis/bot/v2/hook/",
    "https://open.larksuite.com/open-apis/bot/v2/hook/",
)
DINGTALK_WEBHOOK_PREFIX = "https://oapi.dingtalk.com/robot/send"
WECHAT_WEBHOOK_PREFIX = "https://qyapi.weixin.qq.com/cgi-bin/webhook/send"


def is_feishu_webhook(url: str) -> bool:
    u = (url or "").strip().lower()
    return any(u.startswith(p) for p in FEISHU_WEBHOOK_PREFIXES)


def is_dingtalk_webhook(url: str) -> bool:
    return (url or "").strip().lower().startswith(DINGTALK_WEBHOOK_PREFIX)


def is_wechat_webhook(url: str) -> bool:
    return (url or "").strip().lower().startswith(WECHAT_WEBHOOK_PREFIX)


def _fmt_ts(ts: float) -> str:
    import time

    return time.strftime("%Y-%m-%d %H:%M:%S", time.localtime(ts))


def alert_text(alert: "Alert") -> str:
    return (
        "【推理精度异常监控】\n"
        f"告警规则: {alert.rule_name}\n"
        f"实例: {alert.instance}\n"
        f"模型: {alert.model or '-'}\n"
        f"异常类型: {ILL_LABELS.get(alert.ill_type, alert.ill_type)}\n"
        f"窗口计数: {alert.count}\n"
        f"触发时间: {_fmt_ts(alert.ts)}"
    )


def feishu_payload(alert: "Alert") -> Dict[str, Any]:
    """将告警渲染为飞书自定义机器人文本消息。"""
    return {"msg_type": "text", "content": {"text": alert_text(alert)}}


def dingtalk_payload(alert: "Alert") -> Dict[str, Any]:
    """将告警渲染为钉钉自定义机器人文本消息。"""
    return {"msgtype": "text", "text": {"content": alert_text(alert)}}


def wechat_payload(alert: "Alert") -> Dict[str, Any]:
    """将告警渲染为企业微信群机器人文本消息。"""
    return {"msgtype": "text", "text": {"content": alert_text(alert)}}


def generic_payload(alert: "Alert") -> Dict[str, Any]:
    return {
        "rule_name": alert.rule_name,
        "instance": alert.instance,
        "model": alert.model,
        "ill_type": alert.ill_type,
        "count": alert.count,
        "ts": alert.ts,
    }


def webhook_payload(url: str, alert: "Alert") -> Dict[str, Any]:
    """按 webhook 地址自动选择渠道消息格式；未知地址走通用 JSON。"""
    if is_feishu_webhook(url):
        return feishu_payload(alert)
    if is_dingtalk_webhook(url):
        return dingtalk_payload(alert)
    if is_wechat_webhook(url):
        return wechat_payload(alert)
    return generic_payload(alert)


def email_subject(alert: "Alert") -> str:
    return (
        "【推理精度异常监控】"
        f"{alert.rule_name} · {alert.instance} · "
        f"{ILL_LABELS.get(alert.ill_type, alert.ill_type)}"
    )


def _smtp_send_sync(email_cfg: EmailConfig, subject: str, body: str) -> None:
    """同步 SMTP 发送（在 executor 线程中执行，阻塞不卡事件循环）。"""
    import smtplib
    from email.mime.text import MIMEText
    from email.utils import formatdate

    msg = MIMEText(body, "plain", "utf-8")
    msg["Subject"] = subject
    msg["From"] = email_cfg.from_addr
    msg["To"] = ", ".join(email_cfg.to_addrs)
    msg["Date"] = formatdate(localtime=True)

    if email_cfg.use_ssl:
        server = smtplib.SMTP_SSL(email_cfg.smtp_host, email_cfg.smtp_port, timeout=10)
    else:
        server = smtplib.SMTP(email_cfg.smtp_host, email_cfg.smtp_port, timeout=10)
    try:
        if not email_cfg.use_ssl:
            server.starttls()
        if email_cfg.smtp_user:
            server.login(email_cfg.smtp_user, email_cfg.smtp_password)
        server.sendmail(email_cfg.from_addr, list(email_cfg.to_addrs), msg.as_string())
    finally:
        try:
            server.quit()
        except Exception:  # noqa: BLE001 —— 关闭失败忽略
            pass


@dataclass(frozen=True)
class Alert:
    id: int
    rule_name: str
    ts: float
    instance: str
    model: str
    ill_type: str
    count: int

    def to_dict(self) -> Dict[str, Any]:
        return {
            "id": self.id,
            "rule_name": self.rule_name,
            "ts": self.ts,
            "instance": self.instance,
            "model": self.model,
            "ill_type": self.ill_type,
            "count": self.count,
        }


async def _httpx_webhook_sender(url: str, payload: Dict[str, Any]) -> None:
    """默认 webhook 发送器（httpx POST，3s 超时）。异常上抛由引擎捕获。"""
    import httpx

    async with httpx.AsyncClient(timeout=3.0) as client:
        resp = await client.post(url, json=payload)
        resp.raise_for_status()


class AlertEngine:
    def __init__(
        self,
        alerts_buffer: RingBuffer,
        *,
        id_allocer: Optional[Callable[[], int]] = None,
        sender: WebhookSender = _httpx_webhook_sender,
        default_webhook_url: str = "",
        email_cfg: Optional[EmailConfig] = None,
        loop_factory=None,
    ) -> None:
        self._buffer = alerts_buffer
        self._allocer = id_allocer or (lambda: 0)
        self._sender = sender
        self._default_webhook_url = default_webhook_url
        self._email_cfg = email_cfg
        self.rules: List[AlertRule] = []
        # per-rule 滑动窗口队列：rule_index -> normalized_key -> deque[ts]
        self._queues: Dict[int, Dict[tuple, Deque[float]]] = defaultdict(dict)
        self._pending: set = set()
        self._loop = loop_factory

    def set_rules(self, rules: List[AlertRule], default_webhook_url: str = "") -> None:
        """启动时设置规则（alerts 段为非动态段，热重载变更被忽略）。"""
        self.rules = list(rules)
        self._default_webhook_url = default_webhook_url
        self._queues.clear()

    def set_email_cfg(self, email_cfg: Optional[EmailConfig]) -> None:
        self._email_cfg = email_cfg

    def ingest(self, event: AnomalyEvent) -> None:
        for idx, rule in enumerate(self.rules):
            if not rule.enabled:
                continue
            if not rule.matches(event.instance, event.model, event.ill_type):
                continue
            key = (event.instance, event.model, event.ill_type)
            q = self._queues[idx].setdefault(key, deque())
            q.append(event.ts)
            cutoff = event.ts - rule.window_seconds
            while q and q[0] < cutoff:
                q.popleft()
            if len(q) >= rule.threshold:
                alert = Alert(
                    id=self._allocer(),
                    rule_name=rule.name,
                    ts=event.ts,
                    instance=event.instance,
                    model=event.model,
                    ill_type=event.ill_type,
                    count=len(q),
                )
                self._buffer.append(alert)
                q.clear()
                self._dispatch(rule, alert)

    # ------------------------------------------------------------------ #
    # 多渠道分发（fire-and-forget）
    # ------------------------------------------------------------------ #
    def _dispatch(self, rule: AlertRule, alert: Alert) -> None:
        loop = self._loop if self._loop is not None else self._get_loop()

        url = (rule.webhook_url or self._default_webhook_url).strip()
        if url:
            payload = webhook_payload(url, alert)
            self._spawn(loop, self._safe_send(url, payload), f"webhook 调度失败: {url}")

        if self._email_cfg is not None and self._email_cfg.is_ready():
            self._spawn(loop, self._safe_send_email(alert), "邮件告警调度失败")

    def _spawn(self, loop, coro, warn: str) -> None:
        try:
            task = loop.create_task(coro)
        except (RuntimeError, Exception):  # 无运行事件循环等场景：直接跳过
            logger.warning(warn)
            return
        self._pending.add(task)
        task.add_done_callback(self._pending.discard)

    async def _safe_send(self, url: str, payload: Dict[str, Any]) -> None:
        try:
            await self._sender(url, payload)
        except Exception as exc:  # noqa: BLE001 —— 发送失败绝不影响主流程
            logger.error("webhook 发送失败 %s: %s", url, exc)

    async def _safe_send_email(self, alert: Alert) -> None:
        cfg = self._email_cfg
        try:
            loop = asyncio.get_running_loop()
            await loop.run_in_executor(
                None, _smtp_send_sync, cfg, email_subject(alert), alert_text(alert)
            )
        except Exception as exc:  # noqa: BLE001 —— 邮件失败绝不影响主流程
            logger.error("邮件告警发送失败: %s", exc)

    @staticmethod
    def _get_loop():
        try:
            return asyncio.get_running_loop()
        except RuntimeError:
            return asyncio.new_event_loop()

    async def wait_pending(self, timeout: float = 5.0) -> None:
        """等待所有 in-flight webhook（供测试确定性断言）。"""
        if not self._pending:
            return
        await asyncio.wait_for(asyncio.gather(*self._pending, return_exceptions=True), timeout)