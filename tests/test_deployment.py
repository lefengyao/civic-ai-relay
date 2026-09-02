from pathlib import Path


ROOT = Path(__file__).parents[1]


def test_deployment_defaults_to_one_uvicorn_worker():
    compose = (ROOT / "docker-compose.yml").read_text(encoding="utf-8")
    unit = (ROOT / "civic-relay.service").read_text(encoding="utf-8")
    assert "--workers" not in compose
    assert "--workers" not in unit
    assert "./data:/app/data" in compose


def test_env_example_contains_every_required_secret_and_limit():
    env = (ROOT / ".env.example").read_text(encoding="utf-8")
    for name in ("PUBLIC_API_KEY", "ADMIN_API_KEY", "UPSTREAM_API_KEY", "TOKEN_LIMIT_5H", "TOKEN_LIMIT_DAILY"):
        assert f"{name}=" in env


def test_deployment_uses_external_config_path():
    compose = (ROOT / "docker-compose.yml").read_text(encoding="utf-8")
    civic_unit = (ROOT / "civic-relay.service").read_text(encoding="utf-8")
    ai_unit = (ROOT / "ai-relay.service").read_text(encoding="utf-8")

    assert "CIVIC_RELAY_CONFIG_DIR" in compose
    assert "C:/ProgramData/CivicRelay:/app/config" not in compose
    assert "CIVIC_RELAY_CONFIG_FILE: /app/config/relay.env" in compose
    assert "CIVIC_RELAY_CONFIG_FILE=/etc/civic-relay/relay.env" in civic_unit
    assert "CIVIC_RELAY_CONFIG_FILE=/etc/civic-relay/relay.env" in ai_unit
    assert "EnvironmentFile=" not in civic_unit
    assert "EnvironmentFile=" not in ai_unit


def test_dockerfile_runs_as_non_root_and_uses_uv_export():
    dockerfile = (ROOT / "Dockerfile").read_text(encoding="utf-8")
    assert "USER relay" in dockerfile
    assert "--uid 10001" in dockerfile
    assert "requirements.txt" in dockerfile
    assert "tenant_store.py" in dockerfile


def test_container_application_port_is_loopback_only():
    compose = (ROOT / "docker-compose.yml").read_text(encoding="utf-8")

    assert '"127.0.0.1:8000:8000"' in compose
    assert '\n      - "8000:8000"' not in compose


def test_readme_documents_panel_managed_https_proxy():
    readme = (ROOT / "README.md").read_text(encoding="utf-8")

    assert "127.0.0.1:8000" in readme
    assert "反向代理" in readme
    assert "https://<bound-domain>/healthz" in readme
    assert "https://<bound-domain>/v1" in readme


def test_open_source_ignore_rules_exclude_local_secrets_and_private_docs():
    ignore = (ROOT / ".gitignore").read_text(encoding="utf-8")

    for pattern in (".codex/", "docs/", "*-codex-build-prompt.md", "*.ps1", ".env", "data/*.db", "*.pem", "*.key"):
        assert pattern in ignore


def test_readme_uses_application_managed_first_start_without_ignored_scripts():
    readme = (ROOT / "README.md").read_text(encoding="utf-8")

    assert "CIVIC_RELAY_CONFIG_DIR" in readme
    assert "bootstrap-admin-key.txt" in readme
    assert "scripts\\init_config.ps1" not in readme
    assert "scripts\\migrate_config.ps1" not in readme
