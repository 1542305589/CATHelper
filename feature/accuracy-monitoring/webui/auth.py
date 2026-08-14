"""简单登录认证：session token（内存维护），凭据热重载即时生效。

- POST /api/login 校验通过 → 返回短期 token（默认 24h 过期）。
- token 仅为内存映射 token → (user, exp)；不与密码做比对，进程重启即失效。
- 凭据热重载：username/password/password_hash 变更即时生效，已登录 token 不强制失效。
- token 过期无需重启服务，重新登录即可。
"""
from __future__ import annotations

import secrets
import time
from typing import Dict, Optional, Tuple

from .config import AuthConfig


class AuthManager:
    def __init__(self, cfg: AuthConfig) -> None:
        self._cfg = cfg
        self._tokens: Dict[str, Tuple[str, float]] = {}  # token -> (user, expire_ts)

    def configure(self, cfg: AuthConfig) -> None:
        """热重载凭据；不清空已登录 token（到期自然失效）。"""
        self._cfg = cfg

    # ------------------------------------------------------------------ #
    # 登录 / 校验
    # ------------------------------------------------------------------ #
    def authenticate(self, username: str, password: str) -> Optional[str]:
        if not self._cfg.verify(username, password):
            return None
        token = secrets.token_urlsafe(32)
        self._tokens[token] = (username, time.time() + self._cfg.token_ttl_hours * 3600)
        return token

    def validate(self, token: str) -> bool:
        entry = self._tokens.get(token)
        if entry is None:
            return False
        user, expire = entry
        if time.time() >= expire:
            self._tokens.pop(token, None)
            return False
        return True

    def user_for(self, token: str) -> Optional[str]:
        entry = self._tokens.get(token)
        return entry[0] if entry else None

    def clear(self) -> None:
        self._tokens.clear()

    def active_tokens(self) -> int:
        return len(self._tokens)