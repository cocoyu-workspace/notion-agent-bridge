#!/usr/bin/env python3
"""Run an isolated Claude Code process against the local Notion Auto proxy."""
import json
import os
import re
import shlex
import shutil
import subprocess
import sys
import time
import urllib.request
from pathlib import Path
import yaml

ROOT = Path(__file__).resolve().parent
STATUSLINE_SCRIPT = ROOT / "scripts" / "notion-statusline.py"

# Terminal defaults for this launcher only. They are passed as CLI flags to the
# isolated Claude Code process; no global Claude Code settings are modified.
# A user-supplied --model / --effort always wins (no silent substitution).
DEFAULT_MODEL = "sonnet-5.5"
DEFAULT_EFFORT = "low"
DEFAULT_SUBAGENT_MODEL = "sonnet-5.5"
DEFAULT_SUBAGENT_EFFORT = "high"
DEFAULT_AUTO_COMPACT_WINDOW = "200000"


def has_flag(argv, name):
    """True if argv already carries --name or --name=value."""
    return any(a == name or a.startswith(name + "=") for a in argv)


def flag_value(argv, name, default):
    """Effective value of --name (last occurrence wins), else default."""
    value = default
    for i, a in enumerate(argv):
        if a == name and i + 1 < len(argv):
            value = argv[i + 1]
        elif a.startswith(name + "="):
            value = a.split("=", 1)[1]
    return value


def default_model_args(argv):
    """Return the --model/--effort defaults not overridden by argv."""
    out = []
    if not has_flag(argv, "--model"):
        out += ["--model", DEFAULT_MODEL]
    if not has_flag(argv, "--effort"):
        out += ["--effort", DEFAULT_EFFORT]
    return out


# --- permissions ---
# Notion Agent approval_mode -> Claude Code --permission-mode (issue #16, #80).
# Three values are kept apart and never conflated:
#   requested        what the user asked for in this launcher session (flag/env/config; default yolo)
#   sent             what the proxy reports it WILL SEND to Notion (its configured value;
#                    proxy /v1/models header). Not evidence that Notion accepted it.
#   server_effective what Notion's server / workspace actually applies. Nothing readable
#                    exists today (browser preference cannot be read; no live sync), so
#                    it is "unknown" unless a real source passes it in.
# Mapping (session scoped; ~/.claude is never touched):
#   sent ask           -> default            manual approval
#   sent review_for_me -> default (DEGRADED) Notion's reviewer is not Claude's auto
#                                            classifier; auto is never injected
#   sent yolo          -> bypassPermissions  when requested is yolo and server_effective
#                                            has not demoted it to ask/review_for_me
#   missing/unknown anywhere -> default (never relaxes)
# An explicit --permission-mode / --dangerously-skip-permissions always wins.
NOTION_APPROVAL_MODES = ("ask", "review_for_me", "yolo")


def normalize_notion_mode(raw):
    """(mode, known). Empty is yolo/known; anything unrecognised is ask/unknown."""
    if raw is None:
        return "yolo", True
    if not isinstance(raw, str):
        return "ask", False
    value = raw.strip().lower()
    if value == "":
        return "yolo", True
    if value in NOTION_APPROVAL_MODES:
        return value, True
    return "ask", False


def resolve_requested_approval(cli, env, config):
    """Requested Notion mode and its source; priority cli > env > config > default."""
    for source, raw in (("cli", cli), ("env", env), ("config", config)):
        # A --notion-approval flag with an empty/missing value is an explicit but
        # unusable choice: it must not fall through to a looser env/config value.
        if raw is None or (source != "cli" and isinstance(raw, str) and raw.strip() == ""):
            continue
        mode, known = normalize_notion_mode(raw)
        if source == "cli" and isinstance(raw, str) and raw.strip() == "":
            return "ask", "cli-unknown"
        return (mode, source) if known else ("ask", source + "-unknown")
    return "yolo", "default"


def extract_desktop_flags(argv):
    """Return (setup_desktop, launch_desktop) from raw launcher argv."""
    setup = any(a == "--setup-desktop" for a in argv)
    launch = any(a == "--desktop" for a in argv)
    return setup or launch, launch


def extract_launcher_flags(argv):
    """Strip launcher-only flags. Returns (claude_argv, notion_approval, print_permissions)."""
    rest, notion, dry, i = [], None, False, 0
    while i < len(argv):
        a = argv[i]
        if a == "--print-permissions":
            dry = True
        elif a in ("--setup-desktop", "--desktop"):
            pass
        elif a == "--notion-approval":
            nxt = argv[i + 1] if i + 1 < len(argv) else ""
            if nxt.startswith("--"):
                nxt = ""
            else:
                i += 1 if i + 1 < len(argv) else 0
            notion = nxt
        elif a.startswith("--notion-approval="):
            notion = a.split("=", 1)[1]
        else:
            rest.append(a)
        i += 1
    return rest, notion, dry


def explicit_permission_override(argv):
    """Claude permission mode forced by the user's own CLI flags, else None."""
    override = None
    for i, a in enumerate(argv):
        if a == "--permission-mode" and i + 1 < len(argv):
            override = argv[i + 1]
        elif a.startswith("--permission-mode="):
            override = a.split("=", 1)[1]
        elif a == "--dangerously-skip-permissions":
            override = "bypassPermissions"
    return override


def permission_plan(argv, requested_mode="yolo", requested_source="default",
                    sent="yolo", sent_source="default", server_effective=None):
    """Pure mapping from requested/sent/server-effective Notion modes to Claude's mode."""
    notes = []
    sent_known = isinstance(sent, str) and sent.strip().lower() in NOTION_APPROVAL_MODES
    sent_mode = sent.strip().lower() if sent_known else "unknown"
    if not sent_known:
        notes.append("mode the proxy sends to Notion is unknown; using manual approval")
    server = "unknown"
    if isinstance(server_effective, str) and server_effective.strip().lower() in NOTION_APPROVAL_MODES:
        server = server_effective.strip().lower()
    if requested_source.endswith("-unknown"):
        notes.append("requested Notion approval value is unknown; treated as ask")
    if sent_mode != "unknown" and sent_mode != requested_mode:
        notes.append(f"requested {requested_mode} but proxy will send {sent_mode}; restart the proxy to change it")
    degraded = False
    derived = "default"
    if sent_mode == "review_for_me" or server == "review_for_me":
        degraded = True
        notes.append("review_for_me is not equivalent to Claude's auto classifier; degraded to manual "
                     "approval (pass --permission-mode auto yourself to try the classifier; unverified)")
    if server == "review_for_me" and sent_mode == "yolo":
        notes.append("server applies review_for_me although yolo was sent (workspace downgrade)")
    if server == "unknown":
        notes.append("server-effective approval mode is unknown (the browser preference and the workspace "
                     "rule cannot be read; no live sync)")
    if (requested_mode == "yolo" and not requested_source.endswith("-unknown")
            and sent_mode == "yolo" and server in ("unknown", "yolo")):
        derived = "bypassPermissions"
    elif sent_mode == "yolo" or server == "yolo":
        notes.append("yolo is not mapped to local bypass when requested or proxy-sent mode does not "
                     "match yolo or the server demoted it; pass --permission-mode bypassPermissions "
                     "yourself if you want it")
    override = explicit_permission_override(argv)
    if override is not None:
        claude, claude_source = override, "explicit CLI override"
        if override == "bypassPermissions" and server != "yolo":
            notes.append(f"explicit CLI override is looser than sent={sent_mode} "
                         f"server_effective={server}; honoured")
        if override == "auto":
            notes.append("explicit auto uses Claude's classifier, which may be unavailable; not verified here")
    else:
        claude, claude_source = derived, "derived"
    return {"requested_mode": requested_mode, "requested_source": requested_source,
            "sent_mode": sent_mode, "sent_source": sent_source,
            "server_effective": server,
            "server_effective_source": "server-reported" if server != "unknown" else "not readable",
            "claude_permission": claude, "claude_source": claude_source,
            "degraded": degraded, "notes": notes}


def permission_args(plan):
    """CLI flags to inject; empty when the user's own flags already decide."""
    if plan["claude_source"] == "explicit CLI override":
        return []
    return ["--permission-mode", plan["claude_permission"]]


def describe_permissions(plan):
    """One stderr line: where the values came from and what actually applies."""
    line = (f"permissions: requested={plan['requested_mode']} ({plan['requested_source']}) "
            f"sent={plan['sent_mode']} ({plan['sent_source']}) "
            f"server_effective={plan['server_effective']} ({plan['server_effective_source']}) -> "
            f"claude_permission={plan['claude_permission']} ({plan['claude_source']})")
    if plan["degraded"]:
        line += " [degraded]"
    for note in plan["notes"]:
        line += "\n[claude-notion]   note: " + note
    return line



# --- subagents ---
# The Agent tool is hidden by --bare (verified on Claude Code 2.1.280), so the
# launcher runs without it and keeps isolation explicit instead. Subagents run
# locally in Claude Code; the proxy gives each its own Notion thread (keyed by the
# x-claude-code-agent-id request header). Subagent model pinning is NOT claimed.
SUBAGENT_TOOL = "Agent"
BASE_TOOLS = ["Read", "Edit", "Write", "Bash", "Glob", "Grep", "Skill"]


# Optional tools on top of the core set. SendMessage resumes a finished subagent by
# agent ID; verified end to end on Claude Code 2.1.296 (the main agent resumes the same
# subagent and the proxy routes it back to its own Notion thread). Claude Code ignores
# unknown names in --tools (`--tools Read,NoSuchTool` runs normally), so enabling it by
# default is also safe on older CLIs without SendMessage (e.g. 2.1.280).
#   CLAUDE_NOTION_EXTRA_TOOLS=A,B    adds optional tools (deduplicated)
#   CLAUDE_NOTION_DISABLE_TOOLS=A,B  removes optional tools; core tools cannot be
#                                    disabled and naming one is an error. Disable wins.
# A --tools flag passed by the user is appended after the launcher's own and wins.
EXTRA_TOOLS_ENV = "CLAUDE_NOTION_EXTRA_TOOLS"
DISABLE_TOOLS_ENV = "CLAUDE_NOTION_DISABLE_TOOLS"
DEFAULT_EXTRA_TOOLS = ["SendMessage", "ToolSearch"]
DEFAULT_SETTING_SOURCES = "user,project,local"
SUPPORTED_MODELS = [
    "auto",
    "claude-auto",
    "sonnet-5.5",
    "opus-5.5",
    "gpt-6.1-sol",
    "gpt-6.1-sol[1m]",
    "claude-sol-6-1",
    "claude-sol-6-1[1m]",
    "claude-sol-6.1",
    "claude-sol-6.1[1m]",
    "claude-sonnet-5-5",
    "claude-opus-5-5",
]
NOTION_MODEL_PICKER_OPTIONS = [
    {
        "model": "opus-5.5",
        "label": "Opus 5.5（Notion）",
        "description": "Notion Claude Opus 5.5 · 200k context",
        "behavesAs": "claude-opus-5-5",
    },
    {
        "model": "gpt-6.1-sol[1m]",
        "label": "GPT-6.1 Sol（Notion）",
        "description": "Notion GPT-6.1 Sol · 1M context",
        "behavesAs": "claude-opus-5-5[1m]",
    },
    {
        "model": "sonnet-5.5",
        "label": "Sonnet 5.5（Notion）",
        "description": "Notion Claude Sonnet 5.5 · 200k context",
        "behavesAs": "claude-sonnet-5",
    },
]
DEFAULT_MODEL_SETTINGS = {
    "claude-sonnet-5-5": {"autoCompactWindow": int(DEFAULT_AUTO_COMPACT_WINDOW)},
    "claude-opus-5-5": {"autoCompactWindow": int(DEFAULT_AUTO_COMPACT_WINDOW)},
    "sonnet-5.5": {"autoCompactWindow": int(DEFAULT_AUTO_COMPACT_WINDOW)},
    "opus-5.5": {"autoCompactWindow": int(DEFAULT_AUTO_COMPACT_WINDOW)},
}
_TOOL_NAME = re.compile(r"[A-Za-z][A-Za-z0-9_]{0,63}")


def _merge_model_settings(existing=None):
    """Merge 200k autoCompactWindow defaults for Notion Sonnet/Opus 5.5 while preserving user fields."""
    merged = {}
    if isinstance(existing, dict):
        for model_key, val in existing.items():
            if isinstance(val, dict):
                merged[model_key] = dict(val)
    for model_key, defaults in DEFAULT_MODEL_SETTINGS.items():
        entry = dict(merged.get(model_key) or {})
        entry.update(defaults)
        merged[model_key] = entry
    return merged


def _tool_names_from_env(environ, env_name):
    names = []
    for name in environ.get(env_name, "").split(","):
        name = name.strip()
        if not name:
            continue
        if not _TOOL_NAME.fullmatch(name):
            raise ValueError("invalid tool name in " + env_name + ": " + repr(name))
        if name not in names:
            names.append(name)
    return names


def extra_tools_from_env(environ):
    """Validated opt-in tool names (comma separated); never removes base tools."""
    return [n for n in _tool_names_from_env(environ, EXTRA_TOOLS_ENV)
            if n not in BASE_TOOLS + [SUBAGENT_TOOL]]


def disabled_tools_from_env(environ):
    """Validated tool names to switch off; core tools are rejected, not ignored."""
    names = _tool_names_from_env(environ, DISABLE_TOOLS_ENV)
    for name in names:
        if name in BASE_TOOLS + [SUBAGENT_TOOL]:
            raise ValueError("core tool cannot be disabled via " + DISABLE_TOOLS_ENV + ": " + repr(name))
    return names


def effective_extra_tools(environ):
    """Optional tools actually enabled: default + EXTRA - DISABLE (ordered, deduplicated)."""
    disabled = disabled_tools_from_env(environ)
    extra = []
    for name in DEFAULT_EXTRA_TOOLS + extra_tools_from_env(environ):
        if name not in disabled and name not in extra:
            extra.append(name)
    return extra


def subagent_tools(extra=()):
    """Tool allowlist with the Agent tool exposed, plus enabled optional tools."""
    return BASE_TOOLS + [SUBAGENT_TOOL] + [t for t in extra if t not in BASE_TOOLS + [SUBAGENT_TOOL]]


def sent_tools(args):
    """Tool names in the last --tools flag of the final Claude command line."""
    tools = None
    for i, a in enumerate(args):
        if a == "--tools" and i + 1 < len(args):
            tools = args[i + 1]
        elif a.startswith("--tools="):
            tools = a.split("=", 1)[1]
    return [t for t in (tools or "").split(",") if t]


def describe_tools(tools):
    """One-line stderr note naming exactly the tools passed to Claude Code."""
    return "tools sent to Claude (--tools): " + ",".join(tools)


def statusline_command(script_path=STATUSLINE_SCRIPT):
    return "python3 " + shlex.quote(str(script_path))


def subagent_isolation_args(argv=(), base=None, api_key=None, environ=None):
    """Load local Claude Code config (user,project,local) while overriding model,
    availableModels, modelPicker, proxy env, ToolSearch, and statusLine via --settings."""
    model = flag_value(argv, "--model", DEFAULT_MODEL)
    child_env = subagent_env(argv, environ=environ)
    settings_env = dict(child_env)
    settings_env.update({
        "ANTHROPIC_MODEL": model,
        "ANTHROPIC_DEFAULT_MODEL": DEFAULT_MODEL,
        "ANTHROPIC_DEFAULT_SONNET_MODEL": DEFAULT_MODEL,
        "ANTHROPIC_DEFAULT_OPUS_MODEL": "opus-5.5",
        "ANTHROPIC_DEFAULT_HAIKU_MODEL": child_env["CLAUDE_CODE_SUBAGENT_MODEL"],
        "ANTHROPIC_SMALL_FAST_MODEL": child_env["CLAUDE_CODE_SUBAGENT_MODEL"],
        "CLAUDE_CODE_AUTO_MODE_MODEL": child_env["CLAUDE_CODE_SUBAGENT_MODEL"],
        "CLAUDE_CODE_BG_CLASSIFIER_MODEL": child_env["CLAUDE_CODE_SUBAGENT_MODEL"],
        "CLAUDE_CODE_EFFORT_LEVEL": "",
        "CLAUDE_CODE_MAX_CONTEXT_TOKENS": child_env.get("CLAUDE_CODE_MAX_CONTEXT_TOKENS", ""),
        "ANTHROPIC_AUTH_TOKEN": "",
        "ENABLE_TOOL_SEARCH": "true",
        "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
    })
    if base:
        settings_env["ANTHROPIC_BASE_URL"] = base
    if api_key:
        settings_env["ANTHROPIC_API_KEY"] = api_key
    settings = {
        "model": model,
        "availableModels": list(SUPPORTED_MODELS),
        "modelPicker": {
            "replaceBuiltInOptions": True,
            "options": [dict(item) for item in NOTION_MODEL_PICKER_OPTIONS],
        },
        "modelSettings": _merge_model_settings(),
        "env": settings_env,
        "statusLine": {
            "type": "command",
            "command": statusline_command(),
            "padding": 0,
            "refreshInterval": 10,
        },
    }
    out = []
    if not has_flag(argv, "--setting-sources"):
        out += ["--setting-sources", DEFAULT_SETTING_SOURCES]
    out += ["--settings", json.dumps(settings)]
    return out


def subagent_env(argv, environ=None):
    """Child-process env: DEFAULT subagent model = sonnet-5.5 (proxy defaults subagent effort to high)."""
    src = os.environ if environ is None else environ
    model = flag_value(argv, "--model", DEFAULT_MODEL)
    effort = flag_value(argv, "--effort", DEFAULT_EFFORT)
    sol = model.strip().lower() in (
        "gpt-6.1-sol",
        "gpt-6.1-sol[1m]",
        "claude-sol-6-1",
        "claude-sol-6-1[1m]",
        "claude-sol-6.1",
        "claude-sol-6.1[1m]",
        "omniberry-sundae",
    )
    window = src.get("CLAUDE_CODE_AUTO_COMPACT_WINDOW", "").strip() or (
        "1000000" if sol else DEFAULT_AUTO_COMPACT_WINDOW)
    subagent_model = src.get("CLAUDE_CODE_SUBAGENT_MODEL", "").strip() or DEFAULT_SUBAGENT_MODEL
    env = {"CLAUDE_CODE_SUBAGENT_MODEL": subagent_model,
           "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1",
           "CLAUDE_CODE_AUTO_COMPACT_WINDOW": window,
           "ENABLE_TOOL_SEARCH": "true",
           "CLAUDE_NOTION_MODEL": model,
           "CLAUDE_NOTION_EFFORT": effort}
    if sol:
        env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"] = (
            src.get("CLAUDE_CODE_MAX_CONTEXT_TOKENS", "").strip() or "1000000")
    return env


DEFAULT_DESKTOP_3P_DIR = Path.home() / "Library" / "Application Support" / "Claude-3p"
DESKTOP_3P_DIR_ENV = "CLAUDE_DESKTOP_3P_DIR"
# Claude Desktop's index.pre.js rejects configLibrary IDs that do not match /^[a-f0-9-]{36}$/.
DESKTOP_3P_PROFILE_ID = "c8277576-0835-5a50-9864-482124204166"
DESKTOP_3P_PROFILE_NAME = "notion-code-bridge"
DESKTOP_DETECTED_MODELS = [
    "claude-sonnet-5-5",
    "claude-opus-5-5",
    "claude-sol-6-1[1m]",
    "claude-auto",
]
DESKTOP_CUSTOM_MODEL_IDS = [
    "sonnet-5.5",
    "opus-5.5",
    "gpt-6.1-sol[1m]",
    "auto",
]


def resolve_desktop_3p_dir(desktop_dir=None, environ=None):
    if desktop_dir is not None:
        return Path(desktop_dir)
    src = os.environ if environ is None else environ
    raw = src.get(DESKTOP_3P_DIR_ENV, "").strip()
    if raw:
        return Path(raw).expanduser()
    return DEFAULT_DESKTOP_3P_DIR


def resolve_user_claude_settings_path(user_settings_path=None, desktop_dir=None, environ=None):
    if user_settings_path is not None:
        return Path(user_settings_path)
    src = os.environ if environ is None else environ
    if desktop_dir is not None:
        return None
    home = src.get("HOME", "").strip()
    if home:
        return Path(home).expanduser() / ".claude" / "settings.json"
    if src.get(DESKTOP_3P_DIR_ENV, "").strip():
        return None
    return Path.home() / ".claude" / "settings.json"


def _read_json_dict(path):
    if not path.is_file():
        return {}
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
        return data if isinstance(data, dict) else {}
    except (OSError, ValueError):
        return {}


def _write_private_json(path, payload):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(payload, ensure_ascii=False, indent=2) + "\n", encoding="utf-8")
    path.chmod(0o600)


def setup_claude_desktop(base, api_key, desktop_dir=None, environ=None, user_settings_path=None):
    """Write Claude Desktop 3P Gateway configLibrary, claude_desktop_config.json, config.json, and .claude/settings.json."""
    src = os.environ if environ is None else environ
    target_dir = resolve_desktop_3p_dir(desktop_dir=desktop_dir, environ=src)
    target_dir.mkdir(parents=True, exist_ok=True)

    config_path = target_dir / "config.json"
    existing_cfg = _read_json_dict(config_path)
    if config_path.is_file():
        backup_path = target_dir / "config.json.bak"
        shutil.copy2(config_path, backup_path)
        backup_path.chmod(0o600)

    desired_locale = src.get("CLAUDE_DESKTOP_LOCALE", "").strip()
    if desired_locale:
        existing_cfg["locale"] = desired_locale
    elif existing_cfg.get("locale") in (None, "", "en-US"):
        existing_cfg["locale"] = "zh-Hans"

    desktop_env = {
        "ANTHROPIC_BASE_URL": base,
        "ANTHROPIC_AUTH_TOKEN": api_key,
        "ANTHROPIC_API_KEY": api_key,
        "ANTHROPIC_MODEL": "claude-sonnet-5-5",
        "ANTHROPIC_DEFAULT_SONNET_MODEL": "claude-sonnet-5-5",
        "ANTHROPIC_DEFAULT_OPUS_MODEL": "claude-opus-5-5",
        "ANTHROPIC_DEFAULT_HAIKU_MODEL": "claude-sonnet-5-5",
        "ANTHROPIC_SMALL_FAST_MODEL": "claude-sonnet-5-5",
        "CLAUDE_CODE_SUBAGENT_MODEL": "claude-sonnet-5-5",
        "CLAUDE_CODE_AUTO_COMPACT_WINDOW": DEFAULT_AUTO_COMPACT_WINDOW,
        "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
        "ENABLE_TOOL_SEARCH": "true",
        "NO_PROXY": "127.0.0.1,localhost",
    }
    existing_cfg["inferenceConfig"] = {
        "provider": "gateway",
        "modelSource": "static",
        "selectedModel": "claude-sonnet-5-5",
        "detectedModels": list(DESKTOP_DETECTED_MODELS),
        "customModelIds": list(DESKTOP_CUSTOM_MODEL_IDS),
        "env": dict(desktop_env),
    }
    _write_private_json(config_path, existing_cfg)

    desktop_cfg_path = target_dir / "claude_desktop_config.json"
    existing_desktop_cfg = _read_json_dict(desktop_cfg_path)
    existing_desktop_cfg["deploymentMode"] = "3p"
    _write_private_json(desktop_cfg_path, existing_desktop_cfg)

    cred_helper_path = target_dir / "notion-bridge-cred-helper.sh"
    escaped_key = api_key.replace("'", "'\"'\"'")
    cred_helper_path.write_text(f"#!/bin/sh\nprintf '%s\\n' '{escaped_key}'\n", encoding="utf-8")
    cred_helper_path.chmod(0o700)

    config_lib_dir = target_dir / "configLibrary"
    config_lib_dir.mkdir(parents=True, exist_ok=True)
    legacy_non_uuid = config_lib_dir / f"{DESKTOP_3P_PROFILE_NAME}.json"
    if legacy_non_uuid.is_file():
        legacy_non_uuid.unlink()
    meta_path = config_lib_dir / "_meta.json"
    existing_meta = _read_json_dict(meta_path)
    raw_entries = existing_meta.get("entries")
    entries = []
    if isinstance(raw_entries, list):
        for item in raw_entries:
            if not isinstance(item, dict):
                continue
            item_id = str(item.get("id") or "")
            item_name = str(item.get("name") or "")
            if item_id in (DESKTOP_3P_PROFILE_NAME, DESKTOP_3P_PROFILE_ID) or item_name == DESKTOP_3P_PROFILE_NAME:
                continue
            entries.append(item)
    profile_id = DESKTOP_3P_PROFILE_ID
    entries.append({"id": profile_id, "name": DESKTOP_3P_PROFILE_NAME})
    existing_meta["entries"] = entries
    existing_meta["appliedId"] = profile_id
    _write_private_json(meta_path, existing_meta)

    profile_path = config_lib_dir / f"{profile_id}.json"
    profile_payload = {
        "inferenceProvider": "gateway",
        "inferenceGatewayBaseUrl": base,
        "inferenceGatewayAuthScheme": "x-api-key",
        "inferenceCredentialKind": "helper-script",
        "inferenceCredentialHelper": str(cred_helper_path),
        "modelDiscoveryEnabled": True,
        "modelPrefer1mContext": False,
        "defaultModelEffort": "high",
        "scheduledTasksEnabled": True,
        "builtinBrowserEnabled": True,
        "autoModeEnabled": True,
        "keepAwakeEnabled": True,
        "developerModeEnabled": True,
        "skillCreationEnabled": True,
        "toolSearchEnabled": True,
        "isLocalDevMcpEnabled": True,
        "mcpPersistentAlwaysAllowEnabled": True,
        "inferenceModels": [
            {
                "name": "claude-sonnet-5-5",
                "labelOverride": "Claude Sonnet 5.5 (Notion)",
                "supports1m": False,
                "prefer1m": False,
                "anthropicFamilyTier": "sonnet",
                "isFamilyDefault": True,
                "maxEffort": "max",
            },
            {
                "name": "claude-sonnet-5-5",
                "labelOverride": "Claude Sonnet 5.5 (Notion Haiku Tier)",
                "supports1m": False,
                "anthropicFamilyTier": "haiku",
                "isFamilyDefault": True,
                "maxEffort": "max",
            },
            {
                "name": "claude-opus-5-5",
                "labelOverride": "Claude Opus 5.5 (Notion)",
                "supports1m": False,
                "prefer1m": False,
                "anthropicFamilyTier": "opus",
                "isFamilyDefault": True,
                "maxEffort": "max",
            },
            {
                "name": "claude-sol-6-1",
                "labelOverride": "GPT-6.1 Sol (Notion)",
                "supports1m": True,
                "prefer1m": True,
                "maxEffort": "high",
            },
            {
                "name": "claude-auto",
                "labelOverride": "Auto (Notion)",
                "maxEffort": "high",
            },
        ],
    }
    _write_private_json(profile_path, profile_payload)

    settings_path = target_dir / ".claude" / "settings.json"
    existing_settings = _read_json_dict(settings_path)
    settings_env = dict(existing_settings.get("env") or {}) if isinstance(existing_settings.get("env"), dict) else {}
    settings_env.update(desktop_env)
    settings_env.update({
        "ANTHROPIC_DEFAULT_MODEL": "claude-sonnet-5-5",
        "CLAUDE_CODE_AUTO_MODE_MODEL": "claude-sonnet-5-5",
        "CLAUDE_CODE_BG_CLASSIFIER_MODEL": "claude-sonnet-5-5",
        "CLAUDE_CODE_DISABLE_AUTO_MEMORY": "1",
    })
    existing_settings.update({
        "model": "claude-sonnet-5-5",
        "availableModels": list(SUPPORTED_MODELS),
        "modelPicker": {
            "replaceBuiltInOptions": True,
            "options": [dict(item) for item in NOTION_MODEL_PICKER_OPTIONS],
        },
        "modelSettings": _merge_model_settings(existing_settings.get("modelSettings")),
        "env": settings_env,
    })
    _write_private_json(settings_path, existing_settings)

    # Claude Desktop spawns its bundled claude CLI with --setting-sources=user,project,local,
    # which reads ~/.claude/settings.json for per-model autoCompactWindow when CLAUDE_CONFIG_DIR
    # is unset. Merge 200k modelSettings for Notion Sonnet/Opus 5.5 without touching other user keys.
    resolved_user_settings = resolve_user_claude_settings_path(
        user_settings_path=user_settings_path, desktop_dir=desktop_dir, environ=src
    )
    if resolved_user_settings is not None:
        user_settings = _read_json_dict(resolved_user_settings)
        user_settings["modelSettings"] = _merge_model_settings(user_settings.get("modelSettings"))
        _write_private_json(resolved_user_settings, user_settings)

    return {
        "config_path": config_path,
        "desktop_config_path": desktop_cfg_path,
        "cred_helper_path": cred_helper_path,
        "profile_path": profile_path,
        "settings_path": settings_path,
        "user_settings_path": resolved_user_settings,
        "desktop_dir": target_dir,
    }


def _claude_desktop_running_in_1p_mode():
    try:
        out = subprocess.check_output(["ps", "aux"], text=True, stderr=subprocess.DEVNULL)
    except Exception:
        return False
    for line in out.splitlines():
        if "Claude.app/" in line and '"deploymentMode":"1p"' in line:
            return True
    return False


def _claude_desktop_main_running():
    try:
        out = subprocess.check_output(["ps", "aux"], text=True, stderr=subprocess.DEVNULL)
    except Exception:
        return False
    return any("Claude.app/Contents/MacOS/Claude" in line for line in out.splitlines())


def resolve_claude_desktop_app(app_name=None):
    if app_name is not None:
        return app_name
    user_app = Path.home() / "Applications" / "Claude.app"
    if user_app.is_dir():
        return str(user_app)
    return "Claude"


def launch_claude_desktop(app_name=None):
    """Launch Claude Desktop in --3p mode on macOS, restarting if a 1P instance is active."""
    target_app = resolve_claude_desktop_app(app_name)
    if _claude_desktop_running_in_1p_mode():
        subprocess.run(["osascript", "-e", 'tell application "Claude" to quit'],
                       stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
        deadline = time.monotonic() + 10.0
        while time.monotonic() < deadline and _claude_desktop_main_running():
            time.sleep(0.2)
    return subprocess.run(["open", "-a", target_app, "--args", "--3p"], check=True)


DOCUMENTED_SYSTEM_PROMPT = (
    "You are a coding assistant helping the user work in their current directory. "
    "Use local tools to read before editing and to run relevant tests. "
    "Report observed results honestly. "
    "Subagent routing: by default, when the user asks for a subagent "
    "(\"子 Agent\", \"本地子 Agent\", \"subagent\"), call the local Agent tool "
    "(or SendMessage to continue one) so it runs in the local Claude Code "
    "session with local file/shell access; omit the model parameter unless the "
    "user explicitly asks for a supported model, and never select haiku. "
    "Use Notion's built-in cloud subagent (\"Notion 代理\" / \"Notion Spawn Session\") only when the user "
    "explicitly asks for a cloud/Notion subagent (\"云端子 Agent\", \"Notion 子 Agent\", \"cloud subagent\")."
)


DEFAULT_AGENTS_DIR = Path.home() / ".local" / "share" / "notion-code-bridge" / "agents"
AGENTS_DIR_ENV = "CLAUDE_NOTION_AGENTS_DIR"


def load_notion_agents(agents_dir=None, environ=None):
    """Load standalone Claude-Notion subagent definitions from agents_dir/*.md."""
    if agents_dir is None:
        src = os.environ if environ is None else environ
        raw = src.get(AGENTS_DIR_ENV, "").strip()
        agents_dir = Path(raw) if raw else DEFAULT_AGENTS_DIR
    agents_dir = Path(agents_dir)
    if not agents_dir.is_dir():
        return {}
    agents = {}
    for entry in sorted(agents_dir.glob("*.md")):
        if not entry.is_file():
            continue
        try:
            text = entry.read_text(encoding="utf-8")
        except OSError:
            continue
        if not text.startswith("---"):
            continue
        parts = text.split("---", 2)
        if len(parts) < 3:
            continue
        try:
            metadata = yaml.safe_load(parts[1]) or {}
        except yaml.YAMLError:
            continue
        if not isinstance(metadata, dict):
            continue
        name = str(metadata.get("name") or entry.stem).strip()
        description = str(metadata.get("description") or "").strip()
        prompt = parts[2].strip()
        if not name or not description or not prompt:
            continue
        defn = {"description": description, "prompt": prompt}
        model = str(metadata.get("model") or "").strip()
        if model:
            defn["model"] = model
        effort = str(metadata.get("effort") or "").strip()
        if effort:
            defn["effort"] = effort
        tools = metadata.get("tools")
        if isinstance(tools, list) and tools:
            defn["tools"] = [str(t) for t in tools if str(t).strip()]
        agents[name] = defn
    return agents


def build_claude_args(claude, skill_context, argv, plan=None, extra_tools=None, agents=None,
                      base=None, api_key=None):
    """Build the Claude Code command line (pure; no I/O)."""
    plan = plan or permission_plan(argv)
    if extra_tools is None:
        extra_tools = DEFAULT_EXTRA_TOOLS
    agent_args = []
    if agents and not has_flag(argv, "--agents"):
        agent_args = ["--agents", json.dumps(agents, ensure_ascii=False)]
    return ([claude] + subagent_isolation_args(argv, base=base, api_key=api_key) +
            default_model_args(argv) + permission_args(plan) +
            ["--tools", ",".join(subagent_tools(extra_tools)),
             "--system-prompt", DOCUMENTED_SYSTEM_PROMPT,
             "--append-system-prompt", skill_context] + agent_args + list(argv))


def _resolve_config_path(root=ROOT):
    cfg_path = Path(root) / "config.yaml"
    if cfg_path.is_file():
        return cfg_path
    git_marker = Path(root) / ".git"
    if git_marker.is_file():
        try:
            first = git_marker.read_text(encoding="utf-8").splitlines()[0].strip()
            if first.startswith("gitdir:"):
                gitdir = Path(first.split(":", 1)[1].strip())
                if not gitdir.is_absolute():
                    gitdir = (Path(root) / gitdir).resolve()
                # .git/worktrees/<name> -> parent.parent is .git -> parent.parent.parent is main repo
                if gitdir.parent.name == "worktrees":
                    main_cfg = gitdir.parent.parent.parent / "config.yaml"
                    if main_cfg.is_file():
                        return main_cfg
        except OSError:
            pass
    return cfg_path


def main():
    cfg_path = _resolve_config_path(ROOT)
    if not cfg_path.is_file():
        raise SystemExit(f"config.yaml not found at {cfg_path}; initialize notion-manager first")
    cfg = yaml.safe_load(cfg_path.read_text(encoding="utf-8"))
    port = cfg["server"].get("port", "8081")
    base = "http://127.0.0.1:" + str(port)
    key = cfg["server"]["api_key"]
    if not key:
        raise SystemExit("Initialize notion-manager first so config.yaml contains a local API key")
    if not cfg["proxy"].get("agent_thread"):
        raise SystemExit("Enable proxy.agent_thread in config.yaml first")

    raw_argv = sys.argv[1:]
    do_setup_desktop, do_launch_desktop = extract_desktop_flags(raw_argv)
    argv, cli_approval, dry_run = extract_launcher_flags(raw_argv)
    requested, requested_source = resolve_requested_approval(
        cli_approval, os.environ.get("AGENT_APPROVAL_MODE"), cfg["proxy"].get("agent_approval_mode"))

    def probe():
        """Response headers of the running proxy's /v1/models, or None if not ready."""
        req = urllib.request.Request(base + "/v1/models", headers={"Authorization": "Bearer " + key})
        try:
            with urllib.request.urlopen(req, timeout=1) as response:
                return response.headers if response.status == 200 else None
        except Exception:
            return None

    def ready():
        return probe() is not None

    if dry_run or (do_setup_desktop and not do_launch_desktop):
        pass  # dry-run or config-only setup does not start the proxy
    elif not ready():
        log = ROOT / "_claude-notion.log"
        with log.open("ab") as output:
            log.chmod(0o600)
            proxy_env = os.environ.copy()
            for name in ("PORT", "API_KEY", "NOTION_API_BASE", "DEFAULT_MODEL", "AGENT_THREAD"):
                proxy_env.pop(name, None)
            proxy_env["HOST"] = "127.0.0.1"
            proxy_env["AGENT_APPROVAL_MODE"] = requested  # session-scoped Notion mode
            process = subprocess.Popen([str(ROOT / "notion-manager")], cwd=ROOT, env=proxy_env,
                                       stdout=output, stderr=output, start_new_session=True)
        deadline = time.monotonic() + 15
        while not ready():
            if process.poll() is not None or time.monotonic() >= deadline:
                raise SystemExit("Notion proxy failed to start; see " + str(log))
            time.sleep(0.3)
        (ROOT / "_claude-notion.pid").write_text(str(process.pid))

    if do_setup_desktop:
        paths = setup_claude_desktop(base, key)
        print(f"[claude-notion] configured Claude Desktop 3P Gateway in {paths['profile_path']}", file=sys.stderr)
        print(f"[claude-notion] configured Claude Desktop CLI settings in {paths['settings_path']}", file=sys.stderr)
        if do_launch_desktop:
            launch_claude_desktop()
        else:
            print("[claude-notion] launch Claude Desktop with: open -a Claude --args --3p", file=sys.stderr)
        return

    # What the proxy will send to Notion (its configured value). This is NOT a read of
    # Notion's server/workspace effective mode, which cannot be observed from here.
    headers = probe()
    if headers is not None:
        sent = headers.get("X-Agent-Approval-Sent-Mode")
        sent_source = "proxy /v1/models header, configured value to be sent, not server-confirmed (source=%s)" % (
            headers.get("X-Agent-Approval-Sent-Source") or "?")
    else:  # dry run, proxy not running: report what a launcher-started proxy would send
        sent, sent_source = requested, "dry-run: proxy not running, would start with requested mode; not server-confirmed"
    plan = permission_plan(argv, requested, requested_source, sent, sent_source, None)
    if dry_run:
        print(json.dumps(plan, ensure_ascii=False, indent=2))
        return
    print("[claude-notion] " + describe_permissions(plan), file=sys.stderr)

    env = os.environ.copy()
    for name in list(env):
        if name.startswith("ANTHROPIC_") or name == "CLAUDE_CODE_MAX_CONTEXT_TOKENS":
            env.pop(name)
    env.update(ANTHROPIC_BASE_URL=base, ANTHROPIC_API_KEY=key,
               CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC="1",
               NO_PROXY="127.0.0.1,localhost")
    env.update(subagent_env(argv))

    # Advertise extra skills from ~/.agents/skills not already loaded natively by Claude Code.
    # When --setting-sources is empty, include .claude/skills and ~/.claude/skills as well.
    setting_sources = flag_value(argv, "--setting-sources", DEFAULT_SETTING_SOURCES)
    native_skill_names = set()
    if setting_sources.strip():
        for directory in (Path.cwd() / ".claude/skills", Path.home() / ".claude/skills"):
            if directory.is_dir():
                for entry in directory.iterdir():
                    if (entry / "SKILL.md").is_file():
                        native_skill_names.add(entry.name)
        scan_dirs = (Path.home() / ".agents/skills",)
    else:
        scan_dirs = (Path.cwd() / ".claude/skills", Path.home() / ".claude/skills",
                     Path.home() / ".agents/skills")
    skills = {}
    for directory in scan_dirs:
        if not directory.is_dir():
            continue
        for entry in sorted(directory.iterdir()):
            if entry.name in native_skill_names:
                continue
            source = entry / "SKILL.md"
            if not source.is_file():
                continue
            try:
                text = source.read_text()
                if not text.startswith("---"):
                    continue
                metadata = yaml.safe_load(text.split("---", 2)[1]) or {}
                if not isinstance(metadata, dict):
                    continue
                name = str(metadata.get("name") or entry.name)
                if name in native_skill_names:
                    continue
                skills.setdefault(name, (str(metadata.get("description") or ""), str(source.resolve())))
            except (OSError, ValueError, yaml.YAMLError):
                continue
    catalog = "\n".join(f"- {name}: {description}\n  Source: {source}"
                        for name, (description, source) in skills.items())
    skill_context = (
        "Local skills are available below. Select a matching skill by its description. "
        "Use the Skill tool to load an installed Claude skill; if it cannot resolve the name, "
        "use Read on the exact SKILL.md source. Read the skill before following it. "
        "Do not modify the skill source unless the user requests it.\n" + catalog
    )
    claude = shutil.which("claude")
    if not claude:
        raise SystemExit("Claude Code is not installed or is not on PATH")
    try:
        extra_tools = effective_extra_tools(os.environ)
    except ValueError as err:
        raise SystemExit(str(err))
    agents = load_notion_agents()
    args = build_claude_args(claude, skill_context, argv, plan, extra_tools, agents,
                             base=base, api_key=key)
    print("[claude-notion] " + describe_tools(sent_tools(args)), file=sys.stderr)
    if agents and not has_flag(argv, "--agents"):
        print("[claude-notion] subagents (--agents): " + ",".join(agents), file=sys.stderr)
    model = flag_value(argv, "--model", DEFAULT_MODEL)
    effort = flag_value(argv, "--effort", DEFAULT_EFFORT)
    print(f"[claude-notion] model={model} effort={effort} via {base}", file=sys.stderr)
    os.execve(args[0], args, env)


if __name__ == "__main__":
    main()

