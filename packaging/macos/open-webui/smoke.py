"""Exercise an installed slim runtime using disposable data and a fake Ollama.

Run with the locked runtime's Python: python smoke.py /path/to/project.
Does not install services, download models, or contact a real inference server.
"""

from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import importlib.util
import json
import os
from pathlib import Path
import secrets
import socket
import subprocess
import sys
import tempfile
import threading
import time
from urllib.error import HTTPError, URLError
from urllib.request import Request, urlopen


class Ollama(BaseHTTPRequestHandler):
    calls = []

    def log_message(self, *args):
        pass

    def do_GET(self):
        self.reply({"models": [{"name": "sluicework-test:latest", "model": "sluicework-test:latest",
                                "size": 1, "digest": "test", "details": {"family": "qwen"}}]}
                   if self.path == "/api/tags" else {"version": "0.40.16"})

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
        self.calls.append(self.path)
        if self.path == "/api/show":
            self.reply({"capabilities": ["completion"], "details": {"family": "qwen"}, "model_info": {}})
        else:
            self.reply({"model": body.get("model"), "created_at": "2026-11-23T00:00:00Z",
                        "message": {"role": "assistant", "content": "Sluicework smoke OK"},
                        "done": True, "done_reason": "stop", "eval_count": 4, "eval_duration": 1000000})

    def reply(self, value):
        payload = json.dumps(value).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


def exercise(project, directory):
    for package in ("torch", "chromadb", "transformers", "sentence_transformers", "pypdf", "accelerate"):
        assert importlib.util.find_spec(package) is None, f"Unexpected full-profile dependency: {package}"
    # Import the production launch-agent plan so policy drift breaks the smoke test.
    spec = importlib.util.spec_from_file_location("webui_launchd", Path(__file__).with_name("launchd.py"))
    setup = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(setup)
    backend = ThreadingHTTPServer(("127.0.0.1", 0), Ollama)
    thread = threading.Thread(target=backend.serve_forever, daemon=True)
    thread.start()
    state = directory / "state"
    state.mkdir(mode=0o700)
    (state / "data").mkdir()
    with socket.socket() as port_socket:
        port_socket.bind(("127.0.0.1", 0))
        port = port_socket.getsockname()[1]
    origin = f"http://127.0.0.1:{port}"
    plan = setup.agent_specs(state, f"http://127.0.0.1:{backend.server_port}", "", project)
    agent = plan["com.sluicework.open-webui"]
    env = {**agent["EnvironmentVariables"], "HOME": str(directory), "USER_AGENT": "Sluicework-installer-smoke",
           "OPENBLAS_NUM_THREADS": "1", "OMP_NUM_THREADS": "1"}
    assert env["USE_SLIM_DOCKER"] == "true"
    assert env["OFFLINE_MODE"] == "true"
    command = agent["ProgramArguments"][:-1] + [str(port)]
    process = None
    log_path = directory / "startup.log"

    def request(path, body=None, token=None, expected=200, content_type="application/json"):
        headers = {"Content-Type": content_type}
        if token:
            headers["Authorization"] = "Bearer " + token
        payload = json.dumps(body).encode() if isinstance(body, dict) else body
        try:
            with urlopen(Request(origin + path, data=payload, headers=headers), timeout=30) as response:
                status, raw = response.status, response.read()
        except HTTPError as exc:
            status, raw = exc.code, exc.read()
        assert status == expected, f"{path}: expected {expected}, got {status}: {raw[:400]!r}"
        return json.loads(raw) if raw else None

    def start(log):
        child = subprocess.Popen(command, cwd=state, env=env, stdout=log, stderr=subprocess.STDOUT)
        deadline = time.monotonic() + 120
        try:
            while time.monotonic() < deadline:
                if child.poll() is not None:
                    raise RuntimeError("WebUI failed startup; inspect " + str(log_path))
                try:
                    request("/health")
                    return child
                except (URLError, TimeoutError):
                    time.sleep(0.25)
            raise TimeoutError("WebUI health deadline exceeded")
        except BaseException:
            if child.poll() is None:
                child.terminate()
                try:
                    child.wait(timeout=20)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.wait()
            raise

    try:
        with log_path.open("wb") as log:
            process = start(log)
            # No authentication bypass, invalid password/token rejected.
            request("/api/v1/auths/", expected=401)
            password = secrets.token_urlsafe(24)
            account = {"name": "Installer smoke", "email": "smoke@example.com", "password": password}
            signup = request("/api/v1/auths/signup", account)
            assert signup["role"] == "admin"
            request("/api/v1/auths/signin", {**account, "password": "wrong"}, expected=400)
            signin = request("/api/v1/auths/signin", account)
            token = signin["token"]
            request("/api/v1/auths/", token=token)
            request("/api/v1/auths/", token=token[:-4] + "bad!", expected=401)
            # First-user setup automatically closes subsequent public signup.
            request("/api/v1/auths/signup", {**account, "email": "second@example.com"}, expected=403)
            models = request("/api/models", token=token)
            assert any(item["id"] == "sluicework-test:latest" for item in models["data"])
            chat = request("/api/chat/completions", {"model": "sluicework-test:latest", "stream": False,
                           "messages": [{"role": "user", "content": "Installer test"}]}, token)
            assert chat["choices"][0]["message"]["content"] == "Sluicework smoke OK", chat
            assert "/api/chat" in Ollama.calls
            # Slim still stores files; document extraction/RAG requires external services.
            boundary = "sluicework-smoke-boundary"
            content = (f"--{boundary}\r\nContent-Disposition: form-data; name=\"file\"; filename=\"smoke.txt\"\r\n"
                       "Content-Type: text/plain\r\n\r\nSluicework document fixture\r\n"
                       f"--{boundary}--\r\n").encode()
            upload = request("/api/v1/files/?process=false", content, token,
                             content_type="multipart/form-data; boundary=" + boundary)
            assert upload["filename"] == "smoke.txt"
            pdf_content = content.replace(b'smoke.txt', b'smoke.pdf').replace(b'text/plain', b'application/pdf')
            pdf = request("/api/v1/files/?process=false", pdf_content, token,
                          content_type="multipart/form-data; boundary=" + boundary)
            failure = request("/api/v1/retrieval/process/file", {"file_id": pdf["id"]}, token, expected=400)
            assert "external document extractor in slim" in failure["detail"], failure
            # Restart the same data directory, preserving login and files.
            process.terminate()
            process.wait(timeout=30)
            process = start(log)
            request("/api/v1/auths/", token=token)
            request("/api/v1/auths/signin", account)
            files = request("/api/v1/files/", token=token)
            assert any(item["id"] == upload["id"] for item in files["items"]), files
            assert (state / "secret.key").stat().st_mode & 0o777 == 0o600
            print("PASS: slim startup, auth rejection/login, models, chat, file storage, unsupported PDF guidance and restart persistence")
    finally:
        if process and process.poll() is None:
            process.terminate()
            try:
                process.wait(timeout=30)
            except subprocess.TimeoutExpired:
                process.kill()
                process.wait()
        backend.shutdown()
        backend.server_close()


if __name__ == "__main__":
    project = Path(sys.argv[1]).resolve()
    with tempfile.TemporaryDirectory(prefix="sluicework-webui-smoke-") as temporary:
        try:
            exercise(project, Path(temporary))
        except Exception:
            # Disposable credentials only; retain logs locally on failure, never print tokens.
            import shutil
            source = Path(temporary) / "startup.log"
            if source.exists():
                with tempfile.NamedTemporaryFile(prefix="sluicework-webui-failure-", suffix=".log", delete=False) as destination:
                    with source.open("rb") as handle:
                        shutil.copyfileobj(handle, destination)
                print("Startup log saved to", destination.name, file=sys.stderr)
            raise
