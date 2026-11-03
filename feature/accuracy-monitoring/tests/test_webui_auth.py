"""auth 单元测试：登录校验、token 过期、凭据热重载。"""
from __future__ import annotations

import hashlib
import time

from webui.auth import AuthManager
from webui.config import AuthConfig


def _cfg(username="admin", password="", password_hash="", ttl=24):
    if not password and not password_hash:
        password = "s3cret"
    return AuthConfig(username=username, secret=password, password_hash=password_hash,
                      token_ttl_hours=ttl)


def test_login_success_returns_token():
    m = AuthManager(_cfg())
    tok = m.authenticate("admin", "s3cret")
    assert tok
    assert m.validate(tok)
    assert m.user_for(tok) == "admin"


def test_login_fail_wrong_password():
    m = AuthManager(_cfg())
    assert m.authenticate("admin", "wrong") is None


def test_login_fail_wrong_user():
    m = AuthManager(_cfg())
    assert m.authenticate("nobody", "s3cret") is None


def test_password_hash_login():
    h = hashlib.sha256(b"hashpass").hexdigest()
    m = AuthManager(_cfg(password_hash=h))
    assert m.authenticate("admin", "hashpass")
    assert m.authenticate("admin", "wrong") is None


def test_token_expiry():
    m = AuthManager(_cfg(ttl=0))
    # ttl=0 → 立即过期（配置校验挡 >0，此处构造直接验证逻辑）
    m._cfg = AuthConfig(username="admin", secret="x", token_ttl_hours=0)
    tok = m.authenticate("admin", "x")
    assert not m.validate(tok)


def test_validate_unknown_token():
    m = AuthManager(_cfg())
    assert not m.validate("no-such-token")


def test_credential_reload_new_login_uses_new_credentials():
    m = AuthManager(_cfg(password="oldpass"))
    old_tok = m.authenticate("admin", "oldpass")
    assert old_tok
    # 热重载：改密码
    m.configure(AuthConfig(username="admin", secret="newpass", token_ttl_hours=24))
    # 已登录 token 不强制失效
    assert m.validate(old_tok)
    # 旧密码不再可登录，新密码可登录
    assert m.authenticate("admin", "oldpass") is None
    new_tok = m.authenticate("admin", "newpass")
    assert new_tok and new_tok != old_tok


def test_username_reload():
    m = AuthManager(_cfg(username="old"))
    m.configure(AuthConfig(username="new", secret="s3cret", token_ttl_hours=24))
    assert m.authenticate("old", "s3cret") is None
    assert m.authenticate("new", "s3cret")


def test_active_tokens_and_clear():
    m = AuthManager(_cfg())
    m.authenticate("admin", "s3cret")
    m.authenticate("admin", "s3cret")
    assert m.active_tokens() == 2
    m.clear()
    assert m.active_tokens() == 0


def test_token_not_reused_across_sessions():
    m = AuthManager(_cfg())
    t1 = m.authenticate("admin", "s3cret")
    t2 = m.authenticate("admin", "s3cret")
    assert t1 != t2