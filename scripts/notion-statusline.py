#!/usr/bin/env python3
from datetime import datetime, timezone
import hashlib
import json
import os
import re
import subprocess
import sys
import urllib.request

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
NOTION_CHAT_BASE_URL = "https://app.notion.com/chat?t="

C = {
    "cyan": "\033[36m", "brightBlack": "\033[90m", "brightMagenta": "\033[95m",
    "green": "\033[32m", "blue": "\033[34m", "magenta": "\033[35m",
    "brightGreen": "\033[92m",
    "yellow": "\033[33m", "brightBlue": "\033[94m", "red": "\033[31m",
    "purple": "\033[38;2;124;58;237m",
    "reset": "\033[0m",
}

SOL_MODELS = ("gpt-6.1-sol", "gpt-6.1-sol[1m]", "omniberry-sundae")


def c(text, color):
    return f"{C[color]}{text}{C['reset']}"


def turn_count(transcript_path):
    # Tool results also carry type=user and subagent sidechain turns share the file.
    if not transcript_path or not os.path.isfile(transcript_path):
        return None
    n = 0
    try:
        with open(transcript_path, "r") as fh:
            for line in fh:
                line = line.strip()
                if not line:
                    continue
                try:
                    d = json.loads(line)
                except Exception:
                    continue
                if d.get("type") != "user" or d.get("isSidechain") or d.get("isMeta"):
                    continue
                content = d.get("message", {}).get("content")
                if isinstance(content, str):
                    n += 1
                elif isinstance(content, list) and all(
                    isinstance(b, dict) and b.get("type") == "text" for b in content
                ):
                    n += 1
    except Exception:
        return None
    return n


def session_title(transcript_path):
    if not transcript_path or not os.path.isfile(transcript_path):
        return None
    title = None
    try:
        with open(transcript_path, "r") as fh:
            for line in fh:
                if '"custom-title"' not in line:
                    continue
                try:
                    d = json.loads(line)
                except Exception:
                    continue
                if d.get("type") == "custom-title" and d.get("customTitle"):
                    title = d["customTitle"]
    except Exception:
        return None
    return title


def git_info(cwd):
    def run(args):
        try:
            r = subprocess.run(["git", *args], cwd=cwd, capture_output=True, text=True, timeout=2)
            return r.stdout.strip() if r.returncode == 0 else None
        except Exception:
            return None

    branch = run(["rev-parse", "--abbrev-ref", "HEAD"])
    if branch is None:
        return "no git", ""
    stat = run(["diff", "--shortstat", "HEAD"]) or ""
    added = removed = 0
    for tok in stat.split(","):
        tok = tok.strip()
        if "insertion" in tok:
            added = int(tok.split()[0])
        elif "deletion" in tok:
            removed = int(tok.split()[0])
    changes = f"+{added} -{removed}" if (added or removed) else "clean"
    return branch, changes


def fmt_k(n):
    return f"{n / 1000:.0f}k" if isinstance(n, (int, float)) else "?"


def resolve_context(ctx, model_name, env=os.environ, model_id=None):
    used = ctx.get("total_input_tokens")
    pct = ctx.get("used_percentage")
    candidates = (model_id, model_name) if model_id else (model_name, env.get("CLAUDE_NOTION_MODEL"))
    is_sol = any(
        (m or "").strip().lower() in SOL_MODELS or "gpt-6.1 sol" in (m or "").strip().lower()
        for m in candidates
    )
    raw_max = (env.get("CLAUDE_CODE_MAX_CONTEXT_TOKENS") or "").strip() if is_sol else ""
    size = int(raw_max) if raw_max.isdigit() else (1000000 if is_sol else 200000)
    if isinstance(used, (int, float)) and size > 0:
        pct = round(used * 100 / size)
    return fmt_k(used), fmt_k(size), (pct if pct is not None else "?")


def fetch_notion_quota(base_url):
    # Bypass HTTP_PROXY so loopback /health never routes through external proxies.
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    try:
        with opener.open(base_url.rstrip("/") + "/health", timeout=0.3) as resp:
            payload = json.loads(resp.read().decode("utf-8"))
    except Exception:
        return None, None
    quota = payload.get("quota") or []
    if not isinstance(quota, list) or not quota:
        return None, None
    usable = [
        q for q in quota
        if isinstance(q, dict) and q.get("eligible") and not q.get("exhausted") and isinstance(q.get("credit"), dict)
    ]
    acc = (
        next((q for q in usable if q.get("role", "primary") == "primary" and q.get("current")), None)
        or next((q for q in usable if q.get("role", "primary") == "primary"), None)
        or next((q for q in usable if q.get("current")), None)
        or (usable[0] if usable else (quota[0] if isinstance(quota[0], dict) else {}))
    )
    credit = acc.get("credit") or {}
    rolling = (credit.get("rolling") or {}).get("percent")
    billing = (credit.get("billing") or {}).get("percent")
    return rolling, billing


def _parse_updated_at(value):
    """Parse an RFC3339/RFC3339Nano string to a UTC timestamp; 0.0 on any failure."""
    text = str(value or "").strip()
    if not text:
        return 0.0
    if text[-1] in ("Z", "z"):
        text = text[:-1] + "+00:00"
    # fromisoformat on older Pythons rejects fractions longer than 6 digits.
    text = re.sub(r"(\.\d{6})\d+", r"\1", text)
    try:
        dt = datetime.fromisoformat(text)
    except ValueError:
        return 0.0
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    return dt.timestamp()


def _safe_int(value):
    try:
        return int(value or 0)
    except (TypeError, ValueError):
        return 0


def _candidate_state_paths(env=os.environ, state_path=None):
    if state_path:
        return [state_path]
    env_path = (env.get("NOTION_AGENT_THREADS_STATE") or "").strip()
    if env_path:
        return [env_path]
    paths = []
    cfg_path = os.path.join(ROOT, "config.yaml")
    if os.path.isfile(cfg_path):
        try:
            with open(cfg_path, "r") as fh:
                for line in fh:
                    stripped = line.strip()
                    if stripped.startswith("accounts_dir:"):
                        val = stripped.split(":", 1)[1].strip()
                        if val.startswith(("'", '"')):
                            end = val.find(val[0], 1)
                            val = val[1:end] if end > 0 else val[1:]
                        else:
                            val = re.split(r"\s+#", val, maxsplit=1)[0].strip()
                        val = os.path.expanduser(val)
                        if val:
                            acc_dir = val if os.path.isabs(val) else os.path.join(ROOT, val)
                            paths.append(os.path.join(acc_dir, ".agent_threads.state"))
                            break
        except Exception:
            pass
    paths.append(os.path.join(ROOT, "accounts", ".agent_threads.state"))
    paths.append(os.path.expanduser("~/.local/share/notion-code-bridge/accounts/.agent_threads.state"))
    return paths


def resolve_notion_thread_url(session_id, env=os.environ, state_path=None):
    session_id = (session_id or "").strip()
    if not session_id:
        return None
    client_key = hashlib.sha256(f"agent-client:{session_id}".encode("utf-8")).hexdigest()[:32]
    for path in _candidate_state_paths(env=env, state_path=state_path):
        if not path or not os.path.isfile(path):
            continue
        try:
            with open(path, "r") as fh:
                records = json.load(fh)
        except Exception:
            continue
        if not isinstance(records, list):
            continue
        matched = [
            r for r in records
            if isinstance(r, dict) and r.get("client_key") == client_key and r.get("thread_id")
        ]
        if not matched:
            continue
        best = max(
            matched,
            key=lambda r: (_parse_updated_at(r.get("updated_at")), _safe_int(r.get("conv_len"))),
        )
        return NOTION_CHAT_BASE_URL + str(best["thread_id"]).strip().replace("-", "")
    return None


def render_statusline(data, env=os.environ):
    model_obj = data.get("model") or {}
    model_id = (model_obj.get("id") or "").strip() or None
    model = (model_obj.get("display_name") or model_id or env.get("CLAUDE_NOTION_MODEL") or "?").strip() or "?"
    effort = ((data.get("effort") or {}).get("level") or env.get("CLAUDE_NOTION_EFFORT") or "?").strip() or "?"
    ctx_used_k, ctx_size_k, ctx_pct = resolve_context(data.get("context_window") or {}, model, env, model_id=model_id)

    base_url = env.get("ANTHROPIC_BASE_URL") or "http://127.0.0.1:18081"
    six_h, cycle = fetch_notion_quota(base_url)
    if six_h is None and cycle is None:
        rate_limits = data.get("rate_limits") or {}
        six_h = (rate_limits.get("five_hour") or {}).get("used_percentage")
        cycle = (rate_limits.get("seven_day") or {}).get("used_percentage")

    cwd = (data.get("workspace") or {}).get("current_dir") or data.get("cwd", "")
    transcript = data.get("transcript_path") or ""
    session_id = data.get("session_id") or ""
    if not session_id and transcript.endswith(".jsonl"):
        session_id = os.path.basename(transcript)[: -len(".jsonl")]

    turns = turn_count(transcript)
    turns_s = str(turns) if turns is not None else "?"
    branch, changes = git_info(cwd)

    six_h_s = f"{six_h:.1f}%" if isinstance(six_h, (int, float)) else "N/A"
    cycle_s = f"{cycle:.1f}%" if isinstance(cycle, (int, float)) else "N/A"

    line1 = " | ".join([
        c(f"Model: {model}", "cyan"),
        c(f"Turn:{turns_s}", "magenta"),
        c(f"Ctx: {ctx_used_k}/{ctx_size_k} ({ctx_pct}%)", "red"),
        c(f"Effort:{effort}", "brightMagenta"),
        c(f"6h:{six_h_s}", "green"),
        c(f"Cycle: {cycle_s}", "blue"),
        c(f"⎇ {branch}", "purple"),
        c(changes, "yellow"),
    ])
    line2 = c(cwd, "brightBlue")
    title = session_title(transcript)
    if title:
        line2 += "  " + c(title, "brightGreen")
    if session_id:
        line2 += "  " + c(f"resume: {session_id}", "brightBlack")
        thread_url = resolve_notion_thread_url(session_id, env=env)
        if thread_url:
            line2 += "  " + c(f"notion: {thread_url}", "cyan")
    return line1 + "\n" + line2


def main():
    print(render_statusline(json.load(sys.stdin)))


if __name__ == "__main__":
    main()

