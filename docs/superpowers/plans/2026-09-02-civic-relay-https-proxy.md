# Civic Relay HTTPS Reverse-Proxy Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prepare Civic Relay for the existing container-panel HTTPS reverse proxy without exposing the Relay application port to the network.

**Architecture:** Docker publishes Relay only on the host loopback address at `127.0.0.1:8000`. The user's container-panel reverse proxy owns the public domain, certificate, HTTPS redirect, and forwarding to that host-local port. Relay continues to use HTTPS for upstream OpenAI-compatible calls.

**Tech Stack:** Docker Compose, Uvicorn, FastAPI, PowerShell, pytest.

---

## File Structure

- `docker-compose.yml`: Defines the Relay container and binds its application port only to host loopback.
- `tests/test_deployment.py`: Static deployment guardrails for the loopback-only binding and HTTPS proxy instructions.
- `README.md`: Operator instructions for starting the container, configuring the panel proxy, enforcing HTTPS, and smoke-testing the deployed domain.

### Task 1: Restrict The Container Port To The Host

**Files:**
- Modify: `tests/test_deployment.py`
- Modify: `docker-compose.yml`

- [ ] **Step 1: Write the failing deployment guardrail**

Add this test to `tests/test_deployment.py`:

```python
def test_container_application_port_is_loopback_only():
    compose = (ROOT / "docker-compose.yml").read_text(encoding="utf-8")

    assert '"127.0.0.1:8000:8000"' in compose
    assert '\n      - "8000:8000"' not in compose
```

- [ ] **Step 2: Run the test to verify it fails**

Run:

```powershell
.\.venv\Scripts\python.exe -m pytest tests/test_deployment.py::test_container_application_port_is_loopback_only -q
```

Expected: FAIL because Compose currently publishes `8000:8000` on every host interface.

- [ ] **Step 3: Bind only loopback in Compose**

Replace the port mapping in `docker-compose.yml` with:

```yaml
    ports:
      - "127.0.0.1:8000:8000"
```

Keep the existing read-only configuration mount and persistent `./data:/app/data` mount unchanged. Do not add an HTTP reverse-proxy container and do not add a public `ports` mapping.

- [ ] **Step 4: Run the deployment test to verify it passes**

Run:

```powershell
.\.venv\Scripts\python.exe -m pytest tests/test_deployment.py -q
```

Expected: PASS.

- [ ] **Step 5: Check rendered Compose syntax when Docker is available**

Run:

```powershell
docker compose config
```

Expected: the rendered `ports` entry contains `127.0.0.1:8000:8000`; no public `8000:8000` binding appears.

- [ ] **Step 6: Commit**

This workspace is not a Git repository. Do not create a commit; retain the verified working-tree changes.

### Task 2: Document The Panel-Managed HTTPS Route

**Files:**
- Modify: `tests/test_deployment.py`
- Modify: `README.md`

- [ ] **Step 1: Write the failing documentation guardrail**

Add this test to `tests/test_deployment.py`:

```python
def test_readme_documents_panel_managed_https_proxy():
    readme = (ROOT / "README.md").read_text(encoding="utf-8")

    assert "127.0.0.1:8000" in readme
    assert "反向代理" in readme
    assert "https://<bound-domain>/healthz" in readme
    assert "https://<bound-domain>/v1" in readme
```

- [ ] **Step 2: Run the test to verify it fails**

Run:

```powershell
.\.venv\Scripts\python.exe -m pytest tests/test_deployment.py::test_readme_documents_panel_managed_https_proxy -q
```

Expected: FAIL because the README does not yet state the exact panel route and domain verification URLs.

- [ ] **Step 3: Add the HTTPS deployment instructions**

Add a `## Container Panel HTTPS Proxy` section to `README.md` after `## Docker Compose` containing these operator instructions:

```markdown
1. Start the container with `docker compose up -d --build`.
2. In the container panel, create a reverse proxy for the assigned domain.
3. Set the proxy target to `127.0.0.1:8000`.
4. Select or upload the certificate for that exact domain and enable HTTP-to-HTTPS redirect.
5. Do not publish port 8000 to the public network and do not select “不部署证书” for production access.
6. Verify `https://<bound-domain>/healthz`, then configure clients with `https://<bound-domain>/v1`.
7. Restrict `/admin` to an administrator IP allowlist or VPN subnet when the panel supports path-level policy.
```

State that the proxy-to-container hop is host-local HTTP by design; client-to-proxy and Relay-to-upstream paths use HTTPS. State that the panel must own the certificate lifecycle and that an unavailable/invalid certificate must not be bypassed by exposing port 8000.

- [ ] **Step 4: Run the documentation guardrail**

Run:

```powershell
.\.venv\Scripts\python.exe -m pytest tests/test_deployment.py::test_readme_documents_panel_managed_https_proxy -q
```

Expected: PASS.

- [ ] **Step 5: Commit**

This workspace is not a Git repository. Do not create a commit; retain the verified working-tree changes.

### Task 3: Final Deployment Verification

**Files:**
- Verify: `docker-compose.yml`
- Verify: `README.md`
- Verify: `tests/test_deployment.py`

- [ ] **Step 1: Run the full regression suite**

Run:

```powershell
.\.venv\Scripts\python.exe -m pytest -q
```

Expected: all tests pass.

- [ ] **Step 2: Compile all Python modules**

Run:

```powershell
.\.venv\Scripts\python.exe -m compileall app.py admin_api.py admin_ui.py config.py config_store.py db.py limiter.py provider_registry.py runtime.py tenant_store.py upstream.py
```

Expected: exit code 0.

- [ ] **Step 3: Validate the host-local service after container startup**

Run after `docker compose up -d --build`:

```powershell
Invoke-WebRequest -UseBasicParsing http://127.0.0.1:8000/healthz
```

Expected: status code 200 and body `{"status":"ok"}`. Do not send administrator or client credentials over this host-local HTTP check.

- [ ] **Step 4: Validate the panel route after configuring the certificate**

Run with the actual bound domain:

```powershell
Invoke-WebRequest -UseBasicParsing https://<bound-domain>/healthz
```

Expected: status code 200 with a browser-trusted certificate. Verify the panel redirects `http://<bound-domain>/healthz` to HTTPS before sharing the client base URL.

- [ ] **Step 5: Confirm port 8000 is not publicly reachable**

From a machine outside the server host, attempt only the health endpoint at `http://<server-ip>:8000/healthz`.

Expected: connection failure or timeout. The supported route is the domain on HTTPS, not a raw server IP and port.

- [ ] **Step 6: Commit**

This workspace is not a Git repository. Do not create a commit; retain the verified working-tree changes.
