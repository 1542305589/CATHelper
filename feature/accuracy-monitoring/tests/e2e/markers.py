def pytest_configure(config):
    config.addinivalue_line("markers", "P0: priority P0")
    config.addinivalue_line("markers", "P1: priority P1")
    config.addinivalue_line("markers", "P2: priority P2")
    config.addinivalue_line("markers", "lightweight: lightweight tier (PR-triggered)")
    config.addinivalue_line("markers", "full: full tier")
    config.addinivalue_line("markers", "nightly: nightly tier")
    config.addinivalue_line("markers", "inject: requires sidecar injection")
    config.addinivalue_line("markers", "fail_fast: expects service startup failure")
