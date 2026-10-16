import importlib.util
import os
import pathlib
import unittest

spec = importlib.util.spec_from_file_location(
    "claude_notion", pathlib.Path(__file__).with_name("claude-notion.py"))
cn = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cn)  # safe: main() only runs under __main__


def pair(args, flag):
    return args[args.index(flag) + 1]


class LauncherDefaults(unittest.TestCase):
    def test_default_is_sonnet_5_5_low(self):
        args = cn.build_claude_args("claude", "ctx", [])
        self.assertEqual(pair(args, "--model"), "sonnet-5.5")
        self.assertEqual(pair(args, "--effort"), "low")
        self.assertEqual(args.count("--model"), 1)
        self.assertNotIn("opus-5.5", args)

    def test_user_model_wins_no_duplicate_default(self):
        args = cn.build_claude_args("claude", "ctx", ["--model", "auto"])
        self.assertEqual(args.count("--model"), 1)
        self.assertEqual(pair(args, "--model"), "auto")
        self.assertEqual(pair(args, "--effort"), "low")

    def test_equals_forms(self):
        args = cn.build_claude_args("claude", "ctx", ["--model=auto", "--effort=high"])
        self.assertNotIn("--model", args)
        self.assertNotIn("--effort", args)
        self.assertEqual(cn.flag_value(["--model=auto"], "--model", "x"), "auto")

    def test_flag_value_last_wins(self):
        self.assertEqual(cn.flag_value(["--effort", "high", "--effort", "low"], "--effort", "z"), "low")

    def test_global_settings_untouched(self):
        args = cn.build_claude_args("claude", "ctx", [])
        self.assertNotIn("--bare", args)  # --bare hides the Agent tool (#18)
        self.assertEqual(pair(args, "--setting-sources"), "user,project,local")


class ExecutableEntry(unittest.TestCase):
    """claude-notion.py is symlinked as ~/.local/bin/claude-notion, so it must stay executable."""

    path = pathlib.Path(__file__).with_name("claude-notion.py")

    def test_file_is_executable(self):
        self.assertTrue(os.access(self.path, os.X_OK),
                        "claude-notion.py lost its executable bit (run: chmod +x claude-notion.py)")

    def test_shebang(self):
        with open(self.path, "rb") as f:
            self.assertEqual(f.readline().rstrip(b"\r\n"), b"#!/usr/bin/env python3")

    def test_git_index_mode_is_100755(self):
        import shutil
        import subprocess
        if not shutil.which("git"):
            self.skipTest("git not available")
        out = subprocess.run(
            ["git", "ls-files", "-s", "--", self.path.name],
            cwd=self.path.parent, capture_output=True, text=True)
        if out.returncode != 0 or not out.stdout.strip():
            self.skipTest("not a git checkout")
        self.assertEqual(out.stdout.split()[0], "100755",
                         "git mode must be 100755 (run: git update-index --chmod=+x claude-notion.py)")

# --- Permissions tests ---
import importlib.util
import json
import os
import pathlib
import shutil
import stat
import subprocess
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, HTTPServer

HERE = pathlib.Path(__file__).resolve().parent
spec = importlib.util.spec_from_file_location("claude_notion", HERE / "claude-notion.py")
cn = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cn)





def plan(argv=(), cli=None, env=None, cfg=None, sent="from-requested", server=None):
    """Resolve the requested mode like main() does, then derive Claude's.

    sent   = mode the proxy reports it will send (its header); server = server-side effective
    mode from a real Notion source (none is readable today, so main() always passes None).
    """
    req_mode, req_source = cn.resolve_requested_approval(cli, env, cfg)
    if sent == "from-requested":
        sent = req_mode
    return cn.permission_plan(list(argv), req_mode, req_source, sent, "proxy header, configured value to be sent, not server-confirmed", server)


class RequestedSources(unittest.TestCase):
    """Three permission sources + unknown, in priority order cli > env > config > default."""

    def test_default_is_yolo(self):
        self.assertEqual(cn.resolve_requested_approval(None, None, None), ("yolo", "default"))
        self.assertEqual(cn.resolve_requested_approval(None, "  ", ""), ("yolo", "default"))

    def test_config_source(self):
        self.assertEqual(cn.resolve_requested_approval(None, None, "review_for_me"), ("review_for_me", "config"))

    def test_env_beats_config(self):
        self.assertEqual(cn.resolve_requested_approval(None, "ask", "yolo"), ("ask", "env"))

    def test_cli_beats_env_and_config(self):
        self.assertEqual(cn.resolve_requested_approval("yolo", "ask", "ask"), ("yolo", "cli"))

    def test_unknown_never_relaxes(self):
        for bad in ("auto", "yolo!", "bypass", "true", "1", "bypassPermissions"):
            self.assertEqual(cn.resolve_requested_approval(bad, None, "yolo"), ("ask", "cli-unknown"), bad)
            self.assertEqual(cn.resolve_requested_approval(None, bad, None), ("ask", "env-unknown"), bad)
            self.assertEqual(cn.resolve_requested_approval(None, None, bad), ("ask", "config-unknown"), bad)

    def test_non_string_config_value_is_unknown(self):
        self.assertEqual(cn.resolve_requested_approval(None, None, True), ("ask", "config-unknown"))
        self.assertEqual(cn.resolve_requested_approval(None, None, ["yolo"]), ("ask", "config-unknown"))

    def test_case_and_space_tolerated_like_the_proxy(self):
        self.assertEqual(cn.resolve_requested_approval(" YOLO ", None, None), ("yolo", "cli"))


class ClaudeMapping(unittest.TestCase):
    def test_ask_is_manual_default(self):
        p = plan(cli="ask")
        self.assertEqual((p["claude_permission"], p["claude_source"], p["degraded"]), ("default", "derived", False))
        self.assertEqual(cn.permission_args(p), ["--permission-mode", "default"])

    def test_review_for_me_degrades_to_manual_not_auto(self):
        p = plan(cli="review_for_me")
        self.assertEqual(p["claude_permission"], "default")
        self.assertTrue(p["degraded"])
        self.assertNotIn("auto", cn.permission_args(p))
        self.assertTrue(any("not equivalent" in n for n in p["notes"]))

    def test_three_values_are_kept_apart(self):
        p = plan(cli="review_for_me")
        self.assertEqual((p["requested_mode"], p["sent_mode"], p["server_effective"]),
                         ("review_for_me", "review_for_me", "unknown"))
        self.assertIn("not server-confirmed", p["sent_source"])
        self.assertNotIn("notion_effective", p)

    def test_yolo_maps_to_bypass_by_default_and_when_explicit(self):
        for kwargs in ({}, {"cli": "yolo"}, {"env": "yolo"}, {"cfg": "yolo"}):
            p = plan(**kwargs)
            self.assertEqual(p["sent_mode"], "yolo")
            self.assertEqual(p["server_effective"], "unknown")
            self.assertEqual(p["claude_permission"], "bypassPermissions", kwargs)
            self.assertEqual(cn.permission_args(p), ["--permission-mode", "bypassPermissions"])
            self.assertTrue(any("server" in n and "unknown" in n for n in p["notes"]), p["notes"])

    def test_bypass_with_server_confirmed_yolo(self):
        p = plan(cli="yolo", server="yolo")
        self.assertEqual((p["claude_permission"], p["server_effective"]), ("bypassPermissions", "yolo"))
        self.assertEqual(cn.permission_args(p), ["--permission-mode", "bypassPermissions"])

    def test_server_demotion_stays_manual_and_degraded(self):
        p = plan(cli="yolo", server="review_for_me")
        self.assertEqual(p["claude_permission"], "default")
        self.assertTrue(p["degraded"])
        self.assertTrue(any("server" in n and "review_for_me" in n for n in p["notes"]))

    def test_server_yolo_when_ask_requested_stays_manual(self):
        p = plan(cli="ask", server="yolo")
        self.assertEqual(p["claude_permission"], "default")

    def test_server_yolo_but_proxy_does_not_send_yolo_stays_manual(self):
        p = plan(cli="yolo", sent="ask", server="yolo")
        self.assertEqual(p["claude_permission"], "default")
        self.assertTrue(any("will send ask" in n for n in p["notes"]))

    def test_unknown_server_value_is_unknown(self):
        for bad in ("", "bogus", "auto", "bypassPermissions", 7):
            p = plan(cli="yolo", server=bad)
            self.assertEqual(p["server_effective"], "unknown", bad)
            self.assertEqual(p["claude_permission"], "bypassPermissions", bad)

    def test_requested_but_proxy_sends_other_is_reported(self):
        p = plan(cli="yolo", sent="review_for_me")
        self.assertEqual(p["claude_permission"], "default")
        self.assertTrue(any("will send review_for_me" in n for n in p["notes"]))

    def test_unknown_sent_value_is_manual(self):
        for sent in (None, "", "bogus", "auto"):
            p = plan(cli="yolo", sent=sent, server="yolo")
            self.assertEqual(p["claude_permission"], "default", sent)
            self.assertEqual(p["sent_mode"], "unknown")

    def test_unknown_request_is_manual(self):
        p = plan(cli="bypassPermissions")
        self.assertEqual((p["claude_permission"], p["requested_mode"]), ("default", "ask"))

    def test_no_claim_of_browser_sync_or_confirmation_in_report(self):
        text = json.dumps(plan(cli="yolo")).lower() + cn.describe_permissions(plan(cli="yolo")).lower()
        for banned in ("confirmed by notion", "synced", "workspace confirmed"):
            self.assertNotIn(banned, text)


class ExplicitCliOverride(unittest.TestCase):
    def test_permission_mode_flag_wins_nothing_injected(self):
        p = plan(argv=["--permission-mode", "plan"], cli="ask")
        self.assertEqual((p["claude_permission"], p["claude_source"]), ("plan", "explicit CLI override"))
        self.assertEqual(cn.permission_args(p), [])

    def test_permission_mode_equals_form(self):
        p = plan(argv=["--permission-mode=acceptEdits"])
        self.assertEqual((p["claude_permission"], p["claude_source"]), ("acceptEdits", "explicit CLI override"))
        self.assertEqual(cn.permission_args(p), [])

    def test_explicit_auto_is_honoured_but_flagged_unverified(self):
        p = plan(argv=["--permission-mode", "auto"], cli="review_for_me")
        self.assertEqual(p["claude_permission"], "auto")
        self.assertTrue(any("classifier" in n for n in p["notes"]))
        self.assertEqual(cn.permission_args(p), [])

    def test_dangerously_skip_permissions_is_reported_as_bypass_and_not_duplicated(self):
        p = plan(argv=["--dangerously-skip-permissions"], cli="ask")
        self.assertEqual((p["claude_permission"], p["claude_source"]), ("bypassPermissions", "explicit CLI override"))
        self.assertTrue(any("looser" in n for n in p["notes"]))
        self.assertEqual(cn.permission_args(p), [])

    def test_explicit_bypass_override_honoured_even_if_server_effective_unknown(self):
        p = plan(argv=["--permission-mode", "bypassPermissions"], cli="yolo")
        self.assertEqual((p["claude_permission"], p["claude_source"]), ("bypassPermissions", "explicit CLI override"))
        self.assertEqual(cn.permission_args(p), [])
        self.assertTrue(any("looser" in n for n in p["notes"]))

    def test_last_permission_mode_wins(self):
        p = plan(argv=["--permission-mode", "plan", "--permission-mode", "default"])
        self.assertEqual(p["claude_permission"], "default")

    def test_allowed_tools_flags_are_not_a_mode_override(self):
        p = plan(argv=["--allowedTools", "Bash(ls:*)"], cli="ask")
        self.assertEqual(p["claude_source"], "derived")
        self.assertEqual(cn.permission_args(p), ["--permission-mode", "default"])


class BuildArgs(unittest.TestCase):
    def test_default_launch_pins_bypass_permissions(self):
        args = cn.build_claude_args("claude", "ctx", [])
        self.assertEqual(args.count("--permission-mode"), 1)
        self.assertEqual(pair(args, "--permission-mode"), "bypassPermissions")

    def test_explicit_override_not_duplicated_in_args(self):
        argv = ["--permission-mode", "plan"]
        args = cn.build_claude_args("claude", "ctx", argv, plan(argv=argv))
        self.assertEqual(args.count("--permission-mode"), 1)
        self.assertEqual(pair(args, "--permission-mode"), "plan")

    def test_yolo_plan_reaches_args_and_ask_stays_manual(self):
        args = cn.build_claude_args("claude", "ctx", [], plan(cli="yolo"))
        self.assertEqual(pair(args, "--permission-mode"), "bypassPermissions")
        args = cn.build_claude_args("claude", "ctx", [], plan(cli="ask"))
        self.assertEqual(pair(args, "--permission-mode"), "default")

    def test_global_settings_still_untouched(self):
        args = cn.build_claude_args("claude", "ctx", [])
        # #18 replaced --bare; #86 restores local setting sources with highest-priority --settings overrides.
        self.assertNotIn("--bare", args)
        self.assertNotIn("--strict-mcp-config", args)
        settings = json.loads(pair(args, "--settings"))
        self.assertNotIn("hooks", settings)
        self.assertNotIn("disableAllHooks", settings)
        self.assertEqual((settings.get("statusLine") or {}).get("type"), "command")
        self.assertEqual(pair(args, "--setting-sources"), "user,project,local")
        self.assertNotIn("--dangerously-skip-permissions", args)


class LauncherFlags(unittest.TestCase):
    def test_flags_are_stripped_from_claude_argv(self):
        rest, notion, dry = cn.extract_launcher_flags(["--notion-approval", "yolo", "--print-permissions", "-p", "hi"])
        self.assertEqual((rest, notion, dry), (["-p", "hi"], "yolo", True))

    def test_equals_form_and_last_wins(self):
        rest, notion, dry = cn.extract_launcher_flags(["--notion-approval=ask", "--notion-approval=review_for_me", "x"])
        self.assertEqual((rest, notion, dry), (["x"], "review_for_me", False))

    def test_missing_value_is_unknown_not_relaxed(self):
        rest, notion, _ = cn.extract_launcher_flags(["--notion-approval"])
        self.assertEqual(rest, [])
        self.assertEqual(cn.resolve_requested_approval(notion, None, "yolo"), ("ask", "cli-unknown"))


# --- end-to-end launcher plumbing against a fake proxy + fake claude (no Notion, no real CLI) ---

class FakeProxy(BaseHTTPRequestHandler):
    mode_header = None  # None = header absent (old proxy)

    def do_GET(self):
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        if FakeProxy.mode_header is not None:
            self.send_header("X-Agent-Approval-Sent-Mode", FakeProxy.mode_header)
            self.send_header("X-Agent-Approval-Sent-Source", "config")
        self.end_headers()
        self.wfile.write(b'{"object":"list","data":[]}')

    def log_message(self, *a):
        pass


class LauncherEndToEnd(unittest.TestCase):
    def setUp(self):
        self.tmp = pathlib.Path(tempfile.mkdtemp())
        self.addCleanup(shutil.rmtree, self.tmp, True)
        self.server = HTTPServer(("127.0.0.1", 0), FakeProxy)
        threading.Thread(target=self.server.serve_forever, daemon=True).start()
        self.addCleanup(self.server.server_close)
        self.addCleanup(self.server.shutdown)
        shutil.copy(HERE / "claude-notion.py", self.tmp / "claude-notion.py")
        os.chmod(self.tmp / "claude-notion.py", 0o755)
        self.bin = self.tmp / "bin"
        self.bin.mkdir()
        fake = self.bin / "claude"
        fake.write_text("#!/usr/bin/env python3\nimport json,os,sys\n"
                        "open(os.environ['FAKE_ARGV_OUT'],'w').write(json.dumps(sys.argv[1:]))\n")
        fake.chmod(fake.stat().st_mode | stat.S_IXUSR)
        self.argv_out = self.tmp / "argv.json"

    def run_launcher(self, cfg_mode, *argv, proxy_mode="yolo", env_mode=None):
        FakeProxy.mode_header = proxy_mode
        lines = ["server:", "  port: %d" % self.server.server_port, "  api_key: k", "proxy:", "  agent_thread: true"]
        if cfg_mode is not None:
            lines.append("  agent_approval_mode: " + cfg_mode)
        (self.tmp / "config.yaml").write_text("\n".join(lines) + "\n")
        env = {k: v for k, v in os.environ.items() if not k.startswith("ANTHROPIC_") and k != "AGENT_APPROVAL_MODE"}
        env.update(PATH=str(self.bin) + os.pathsep + env["PATH"], FAKE_ARGV_OUT=str(self.argv_out),
                   HOME=str(self.tmp))
        if env_mode:
            env["AGENT_APPROVAL_MODE"] = env_mode
        return subprocess.run([sys.executable, str(self.tmp / "claude-notion.py"), *argv], env=env,
                              capture_output=True, text=True, cwd=self.tmp, timeout=30)

    def claude_argv(self):
        return json.loads(self.argv_out.read_text())

    def test_default_yolo_launch_passes_bypass_mode_and_reports(self):
        r = self.run_launcher(None)
        self.assertEqual(r.returncode, 0, r.stderr)
        argv = self.claude_argv()
        self.assertEqual(pair(argv, "--permission-mode"), "bypassPermissions")
        self.assertIn("sent=yolo", r.stderr)
        self.assertIn("claude_permission=bypassPermissions", r.stderr)

    def test_launcher_flags_not_forwarded_to_claude(self):
        r = self.run_launcher(None, "--notion-approval", "ask", "-p", "hi", proxy_mode="ask")
        self.assertEqual(r.returncode, 0, r.stderr)
        argv = self.claude_argv()
        self.assertNotIn("--notion-approval", argv)
        self.assertIn("-p", argv)
        self.assertEqual(pair(argv, "--permission-mode"), "default")

    def test_yolo_sent_by_proxy_maps_to_bypass_permissions(self):
        r = self.run_launcher("yolo", proxy_mode="yolo")
        self.assertEqual(pair(self.claude_argv(), "--permission-mode"), "bypassPermissions")
        self.assertIn("requested=yolo", r.stderr)
        self.assertIn("sent=yolo", r.stderr)
        self.assertIn("server_effective=unknown", r.stderr)
        self.assertIn("source=config", r.stderr)

    def test_yolo_requested_but_proxy_reports_ask_stays_manual(self):
        r = self.run_launcher(None, "--notion-approval", "yolo", proxy_mode="ask")
        self.assertEqual(pair(self.claude_argv(), "--permission-mode"), "default")
        self.assertIn("will send ask", r.stderr)

    def test_old_proxy_without_header_is_unknown_and_manual(self):
        r = self.run_launcher("yolo", proxy_mode=None)
        self.assertEqual(pair(self.claude_argv(), "--permission-mode"), "default")
        self.assertIn("sent=unknown", r.stderr)

    def test_review_for_me_is_reported_degraded(self):
        r = self.run_launcher("review_for_me", proxy_mode="review_for_me")
        self.assertEqual(pair(self.claude_argv(), "--permission-mode"), "default")
        self.assertIn("degraded", r.stderr)

    def test_explicit_cli_permission_mode_respected(self):
        r = self.run_launcher(None, "--permission-mode", "plan")
        argv = self.claude_argv()
        self.assertEqual(argv.count("--permission-mode"), 1)
        self.assertEqual(pair(argv, "--permission-mode"), "plan")
        self.assertIn("explicit CLI override", r.stderr)

    def test_print_permissions_is_a_dry_run_json_report(self):
        r = self.run_launcher("review_for_me", "--print-permissions", proxy_mode="review_for_me")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertFalse(self.argv_out.exists(), "dry run must not exec claude")
        report = json.loads(r.stdout)
        self.assertEqual(report["requested_mode"], "review_for_me")
        self.assertEqual(report["requested_source"], "config")
        self.assertEqual(report["sent_mode"], "review_for_me")
        self.assertEqual(report["server_effective"], "unknown")
        self.assertEqual(report["claude_permission"], "default")
        self.assertTrue(report["degraded"])
        self.assertNotIn("k", [report.get("api_key")])
        self.assertNotIn("api_key", json.dumps(report).lower())

    def test_unknown_config_value_is_manual_and_visible(self):
        r = self.run_launcher("bypassPermissions", "--print-permissions", proxy_mode="ask")
        report = json.loads(r.stdout)
        self.assertEqual(report["requested_source"], "config-unknown")
        self.assertEqual(report["claude_permission"], "default")

    def test_env_source(self):
        r = self.run_launcher(None, "--print-permissions", proxy_mode="ask", env_mode="review_for_me")
        self.assertEqual(json.loads(r.stdout)["requested_source"], "env")

    def test_setup_desktop_writes_files_and_does_not_exec_claude(self):
        desktop_dir = self.tmp / "Claude-3p-e2e"
        os.environ["CLAUDE_DESKTOP_3P_DIR"] = str(desktop_dir)
        self.addCleanup(os.environ.pop, "CLAUDE_DESKTOP_3P_DIR", None)
        r = self.run_launcher(None, "--setup-desktop")
        self.assertEqual(r.returncode, 0, r.stderr)
        self.assertFalse(self.argv_out.exists(), "--setup-desktop must not exec claude CLI")
        cfg = json.loads((desktop_dir / "config.json").read_text(encoding="utf-8"))
        self.assertEqual(cfg["inferenceConfig"]["provider"], "gateway")
        self.assertEqual(cfg["inferenceConfig"]["selectedModel"], "claude-sonnet-5-5")
        self.assertTrue((desktop_dir / ".claude" / "settings.json").is_file())


# --- Subagents & Statusline tests ---
import importlib.util
import json
import pathlib
import unittest

spec = importlib.util.spec_from_file_location(
    "claude_notion_sub", pathlib.Path(__file__).with_name("claude-notion.py"))
cn = importlib.util.module_from_spec(spec)
spec.loader.exec_module(cn)





class SubagentLauncher(unittest.TestCase):
    def args(self, argv=()):
        return cn.build_claude_args("claude", "ctx", list(argv))

    def test_agent_tool_is_exposed(self):
        tools = pair(self.args(), "--tools").split(",")
        self.assertIn("Agent", tools)
        for base in ("Read", "Edit", "Write", "Bash", "Glob", "Grep", "Skill"):
            self.assertIn(base, tools)

    def test_bare_is_not_used_because_it_hides_the_agent_tool(self):
        self.assertNotIn("--bare", self.args())

    def test_isolation_survives_without_bare(self):
        args = cn.build_claude_args("claude", "ctx", [], base="http://127.0.0.1:18081", api_key="k123")
        self.assertEqual(pair(args, "--setting-sources"), "user,project,local")
        self.assertNotIn("--strict-mcp-config", args)
        settings = json.loads(pair(args, "--settings"))
        self.assertNotIn("hooks", settings)
        self.assertNotIn("disableAllHooks", settings)
        self.assertEqual(settings.get("model"), "sonnet-5.5")
        self.assertIn("sonnet-5.5", settings.get("availableModels", []))
        self.assertIn("opus-5.5", settings.get("availableModels", []))
        self.assertIn("gpt-6.1-sol", settings.get("availableModels", []))
        picker = settings.get("modelPicker") or {}
        self.assertTrue(picker.get("replaceBuiltInOptions"))
        picker_opts = picker.get("options") or []
        self.assertEqual(
            [(o.get("label"), o.get("model")) for o in picker_opts],
            [
                ("Opus 5.5（Notion）", "opus-5.5"),
                ("GPT-6.1 Sol（Notion）", "gpt-6.1-sol[1m]"),
                ("Sonnet 5.5（Notion）", "sonnet-5.5"),
            ],
        )
        env = settings.get("env") or {}
        self.assertEqual(env.get("ANTHROPIC_BASE_URL"), "http://127.0.0.1:18081")
        self.assertEqual(env.get("ANTHROPIC_API_KEY"), "k123")
        self.assertEqual(env.get("ANTHROPIC_AUTH_TOKEN"), "")
        self.assertEqual(env.get("ANTHROPIC_MODEL"), "sonnet-5.5")
        self.assertEqual(env.get("ANTHROPIC_DEFAULT_MODEL"), "sonnet-5.5")
        self.assertEqual(env.get("ANTHROPIC_DEFAULT_SONNET_MODEL"), "sonnet-5.5")
        self.assertEqual(env.get("ANTHROPIC_DEFAULT_OPUS_MODEL"), "opus-5.5")
        self.assertEqual(env.get("ANTHROPIC_DEFAULT_HAIKU_MODEL"), "sonnet-5.5")
        self.assertEqual(env.get("ENABLE_TOOL_SEARCH"), "true")
        statusline = settings.get("statusLine") or {}
        self.assertEqual(statusline.get("type"), "command")
        self.assertIn("scripts/notion-statusline.py", statusline.get("command", ""))
        self.assertEqual(statusline.get("padding"), 0)
        self.assertEqual(statusline.get("refreshInterval"), 10)

    # Default subagent model is sonnet-5.5 (proxy defaults subagent effort to high);
    # an explicit CLAUDE_CODE_SUBAGENT_MODEL in the environment overrides it.
    def test_subagent_model_default_is_sonnet_5_5_high_without_haiku_alias(self):
        self.assertEqual(cn.DEFAULT_SUBAGENT_MODEL, "sonnet-5.5")
        self.assertEqual(cn.DEFAULT_SUBAGENT_EFFORT, "high")
        env = cn.subagent_env([], environ={})
        self.assertEqual(env["CLAUDE_CODE_SUBAGENT_MODEL"], "sonnet-5.5")
        self.assertEqual(env["CLAUDE_NOTION_MODEL"], "sonnet-5.5")
        self.assertEqual(env["CLAUDE_NOTION_EFFORT"], "low")
        env = cn.subagent_env(["--model", "auto"], environ={})
        self.assertEqual(env["CLAUDE_CODE_SUBAGENT_MODEL"], "sonnet-5.5")
        self.assertEqual(env["CLAUDE_NOTION_MODEL"], "auto")
        env = cn.subagent_env(["--model=opus-5.5", "--effort=high"], environ={})
        self.assertEqual(env["CLAUDE_CODE_SUBAGENT_MODEL"], "sonnet-5.5")
        self.assertEqual(env["CLAUDE_NOTION_MODEL"], "opus-5.5")
        self.assertEqual(env["CLAUDE_NOTION_EFFORT"], "high")
        env = cn.subagent_env(["--model", "auto"], environ={"CLAUDE_CODE_SUBAGENT_MODEL": "opus-5.5"})
        self.assertEqual(env["CLAUDE_CODE_SUBAGENT_MODEL"], "opus-5.5")
        for value in env.values():
            self.assertNotIn("haiku", value.lower())

    def test_auto_memory_is_off_without_bare(self):
        self.assertEqual(cn.subagent_env([])["CLAUDE_CODE_DISABLE_AUTO_MEMORY"], "1")

    def test_sol_window_and_explicit_limits(self):
        for model in ("gpt-6.1-sol", "gpt-6.1-sol[1m]", "omniberry-sundae"):
            env = cn.subagent_env(["--model", model], environ={})
            self.assertEqual(env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"], "1000000")
            self.assertEqual(env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"], "1000000")
            self.assertEqual(env["CLAUDE_CODE_SUBAGENT_MODEL"], "sonnet-5.5")
        env = cn.subagent_env(["--model", "gpt-6.1-sol"], environ={
            "CLAUDE_CODE_MAX_CONTEXT_TOKENS": "800000",
            "CLAUDE_CODE_AUTO_COMPACT_WINDOW": "700000"})
        self.assertEqual(env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"], "800000")
        self.assertEqual(env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"], "700000")
        self.assertNotIn("CLAUDE_CODE_MAX_CONTEXT_TOKENS", cn.subagent_env([], environ={}))

    def test_auto_compact_window_default_fits_notion_budget_and_honours_override(self):
        env = cn.subagent_env([], environ={})
        self.assertEqual(env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"], "200000")
        env = cn.subagent_env([], environ={"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "25000"})
        self.assertEqual(env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"], "25000")
        env = cn.subagent_env([], environ={"CLAUDE_CODE_AUTO_COMPACT_WINDOW": "   "})
        self.assertEqual(env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"], "200000")

    def test_prompt_routes_default_and_local_subagents_to_agent_tool_and_cloud_to_notion(self):
        prompt = pair(self.args(), "--system-prompt")
        for want in ("本地子 Agent", "云端子 Agent", "Notion Spawn Session", "Agent", "SendMessage", "haiku"):
            self.assertIn(want, prompt)


class DefaultAndExtraTools(unittest.TestCase):
    """SendMessage and ToolSearch are on by default and can be switched off
    with CLAUDE_NOTION_DISABLE_TOOLS."""

    CORE = "Read,Edit,Write,Bash,Glob,Grep,Skill,Agent"
    DEFAULT = CORE + ",SendMessage,ToolSearch"

    def tools(self, **kw):
        return pair(cn.build_claude_args("claude", "ctx", [], **kw), "--tools")

    def effective(self, **env):
        return cn.effective_extra_tools(env)

    def test_default_allowlist_includes_send_message_after_agent(self):
        self.assertEqual(self.tools(), self.DEFAULT)
        self.assertEqual(self.effective(), ["SendMessage", "ToolSearch"])

    def test_extra_tools_env_parsing_is_unchanged(self):
        self.assertEqual(cn.extra_tools_from_env({}), [])
        self.assertEqual(cn.extra_tools_from_env({"CLAUDE_NOTION_EXTRA_TOOLS": "SendMessage"}), ["SendMessage"])
        self.assertEqual(cn.extra_tools_from_env({"CLAUDE_NOTION_EXTRA_TOOLS": "  ,, "}), [])

    def test_extra_tools_dedupe_against_default_and_never_remove_core(self):
        extra = self.effective(CLAUDE_NOTION_EXTRA_TOOLS=" SendMessage, ListAgents ,SendMessage,Agent,Read")
        self.assertEqual(extra, ["SendMessage", "ToolSearch", "ListAgents"])
        self.assertEqual(self.tools(extra_tools=extra), self.DEFAULT + ",ListAgents")

    def test_invalid_extra_names_are_rejected(self):
        for bad in ("Send Message", "Bash(rm -rf *)", "a;b", "--bare", "x" * 65, "1abc"):
            with self.assertRaises(ValueError, msg=bad):
                self.effective(CLAUDE_NOTION_EXTRA_TOOLS=bad)

    def test_disable_send_message(self):
        extra = self.effective(CLAUDE_NOTION_DISABLE_TOOLS="SendMessage,ToolSearch")
        self.assertEqual(extra, [])
        self.assertEqual(self.tools(extra_tools=extra), self.CORE)

    def test_disable_accepts_lists_blanks_and_unknown_optional_names(self):
        self.assertEqual(self.effective(CLAUDE_NOTION_DISABLE_TOOLS=" SendMessage , ToolSearch, ,NoSuch,SendMessage"), [])
        self.assertEqual(self.effective(CLAUDE_NOTION_DISABLE_TOOLS="  ,, "), ["SendMessage", "ToolSearch"])

    def test_disable_wins_over_extra(self):
        extra = self.effective(CLAUDE_NOTION_EXTRA_TOOLS="SendMessage,ListAgents",
                               CLAUDE_NOTION_DISABLE_TOOLS="SendMessage")
        self.assertEqual(extra, ["ToolSearch", "ListAgents"])

    def test_core_tools_cannot_be_disabled(self):
        for core in ("Read", "Edit", "Write", "Bash", "Glob", "Grep", "Skill", "Agent"):
            with self.assertRaises(ValueError, msg=core):
                self.effective(CLAUDE_NOTION_DISABLE_TOOLS="SendMessage," + core)

    def test_invalid_disable_names_are_rejected(self):
        for bad in ("Send Message", "Bash(rm -rf *)", "a;b", "--bare", "x" * 65, "1abc"):
            with self.assertRaises(ValueError, msg=bad):
                self.effective(CLAUDE_NOTION_DISABLE_TOOLS=bad)

    def test_user_tools_flag_still_comes_last_so_the_user_wins(self):
        args = cn.build_claude_args("claude", "ctx", ["--tools", "Read"])
        self.assertEqual(args[-2:], ["--tools", "Read"])
        self.assertGreater(len(args) - 2, args.index("--tools"))

    def test_no_fake_resume_argument_or_agents_flag_is_injected(self):
        args = cn.build_claude_args("claude", "ctx", [])
        self.assertNotIn("--agents", args)
        self.assertFalse([a for a in args if "resume" in a.lower()])

    def test_loads_standalone_notion_agents_and_injects_agents_flag(self):
        import tempfile
        with tempfile.TemporaryDirectory() as tmp:
            root = pathlib.Path(tmp)
            (root / "coder.md").write_text(
                "---\nname: coder\ndescription: coder desc\nmodel: sonnet-5.5\neffort: high\n---\n\ncoder body\n",
                encoding="utf-8",
            )
            (root / "scout.md").write_text(
                "---\nname: scout\ndescription: scout desc\nmodel: sonnet-5.5\neffort: high\n---\n\nscout body\n",
                encoding="utf-8",
            )
            agents = cn.load_notion_agents(root)
            self.assertEqual(set(agents), {"coder", "scout"})
            self.assertEqual(agents["coder"], {
                "description": "coder desc",
                "prompt": "coder body",
                "model": "sonnet-5.5",
                "effort": "high",
            })
            args = cn.build_claude_args("claude", "ctx", [], agents=agents)
            self.assertEqual(json.loads(pair(args, "--agents")), agents)
            user_args = cn.build_claude_args("claude", "ctx", ["--agents", '{"custom":{}}'], agents=agents)
            self.assertEqual(user_args.count("--agents"), 1)
            self.assertEqual(pair(user_args, "--agents"), '{"custom":{}}')


    def test_tools_note_reflects_the_names_actually_sent(self):
        args = cn.build_claude_args("claude", "ctx", [])
        note = cn.describe_tools(pair(args, "--tools").split(","))
        self.assertIn("Agent", note)
        self.assertIn("SendMessage", note)
        self.assertNotIn("Task", note)
        self.assertNotIn("server-effective", note)
        self.assertIn(pair(args, "--tools"), note)

    def test_tools_note_follows_the_user_tools_flag(self):
        args = cn.build_claude_args("claude", "ctx", ["--tools=Read,Bash"])
        self.assertEqual(cn.sent_tools(args), ["Read", "Bash"])
        args = cn.build_claude_args("claude", "ctx", ["--tools", "Read"])
        self.assertEqual(cn.sent_tools(args), ["Read"])
        self.assertEqual(cn.sent_tools(cn.build_claude_args("claude", "ctx", [])), self.DEFAULT.split(","))


class NotionStatuslineTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        sl_spec = importlib.util.spec_from_file_location(
            "notion_statusline",
            pathlib.Path(__file__).with_name("scripts") / "notion-statusline.py",
        )
        cls.sl = importlib.util.module_from_spec(sl_spec)
        sl_spec.loader.exec_module(cls.sl)

    def test_renders_sonnet_200k_and_sol_1000k_with_notion_quota(self):
        import http.server
        import threading

        health_payload = {
            "quota": [{
                "eligible": True,
                "exhausted": False,
                "credit": {
                    "rolling": {"percent": 31.2},
                    "billing": {"percent": 34.24},
                },
            }]
        }

        class Handler(http.server.BaseHTTPRequestHandler):
            def do_GET(self):
                body = json.dumps(health_payload).encode("utf-8")
                self.send_response(200)
                self.send_header("Content-Type", "application/json")
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

            def log_message(self, *args):
                pass

        srv = http.server.HTTPServer(("127.0.0.1", 0), Handler)
        t = threading.Thread(target=srv.serve_forever, daemon=True)
        t.start()
        try:
            base = f"http://127.0.0.1:{srv.server_port}"
            out_sonnet = self.sl.render_statusline(
                {
                    "model": {"display_name": "sonnet-5.5"},
                    "effort": {"level": "low"},
                    "context_window": {
                        "total_input_tokens": 22000,
                        "context_window_size": 200000,
                        "used_percentage": 11,
                    },
                    "cwd": str(pathlib.Path(__file__).parent),
                    "session_id": "abc-123",
                },
                env={"ANTHROPIC_BASE_URL": base},
            )
            self.assertIn("Model: sonnet-5.5", out_sonnet)
            self.assertIn("Ctx: 22k/200k (11%)", out_sonnet)
            self.assertIn("Effort:low", out_sonnet)
            self.assertIn("6h:31.2%", out_sonnet)
            self.assertIn("Cycle: 34.2%", out_sonnet)
            self.assertIn("resume: abc-123", out_sonnet)

            out_sol = self.sl.render_statusline(
                {
                    "model": {"display_name": "gpt-6.1-sol"},
                    "effort": {"level": "high"},
                    "context_window": {
                        "total_input_tokens": 50000,
                        "context_window_size": 200000,
                        "used_percentage": 25,
                    },
                    "cwd": str(pathlib.Path(__file__).parent),
                },
                env={"ANTHROPIC_BASE_URL": base, "CLAUDE_NOTION_MODEL": "gpt-6.1-sol"},
            )
            self.assertIn("Model: gpt-6.1-sol", out_sol)
            self.assertIn("Ctx: 50k/1000k (5%)", out_sol)
            self.assertIn("Effort:high", out_sol)
            self.assertIn("6h:31.2%", out_sol)
            self.assertIn("Cycle: 34.2%", out_sol)

            # Mid-session /model switch from sonnet-5.5 to GPT-6.1 Sol（Notion）
            out_switched_sol = self.sl.render_statusline(
                {
                    "model": {"id": "gpt-6.1-sol[1m]", "display_name": "GPT-6.1 Sol（Notion）"},
                    "effort": {"level": "low"},
                    "context_window": {"total_input_tokens": 50000},
                    "cwd": str(pathlib.Path(__file__).parent),
                },
                env={"ANTHROPIC_BASE_URL": base, "CLAUDE_NOTION_MODEL": "sonnet-5.5"},
            )
            self.assertIn("Model: GPT-6.1 Sol（Notion）", out_switched_sol)
            self.assertIn("Ctx: 50k/1000k (5%)", out_switched_sol)

            # Mid-session /model switch from gpt-6.1-sol back to Sonnet 5.5（Notion）
            out_switched_sonnet = self.sl.render_statusline(
                {
                    "model": {"id": "sonnet-5.5", "display_name": "Sonnet 5.5（Notion）"},
                    "effort": {"level": "low"},
                    "context_window": {"total_input_tokens": 50000},
                    "cwd": str(pathlib.Path(__file__).parent),
                },
                env={"ANTHROPIC_BASE_URL": base, "CLAUDE_NOTION_MODEL": "gpt-6.1-sol"},
            )
            self.assertIn("Model: Sonnet 5.5（Notion）", out_switched_sonnet)
            self.assertIn("Ctx: 50k/200k (25%)", out_switched_sonnet)
        finally:
            srv.shutdown()
            srv.server_close()

    def test_render_statusline_displays_notion_thread_url(self):
        import hashlib
        session_id = "6f54f8dd-b3b9-4c9a-8284-0e7e2a9b20e6"
        client_key = hashlib.sha256(f"agent-client:{session_id}".encode("utf-8")).hexdigest()[:32]
        with tempfile.TemporaryDirectory() as td:
            state_file = pathlib.Path(td) / ".agent_threads.state"
            state_file.write_text(
                json.dumps(
                    [
                        {
                            "client_key": client_key,
                            "thread_id": "11111111-2222-3333-4444-555555555555",
                            "conv_len": 3,
                            "updated_at": "2026-10-15T00:00:00Z",
                        },
                        {
                            "client_key": client_key,
                            "thread_id": "bda42e88-734b-4ce2-a0bc-75dccb01fd4a",
                            "conv_len": 45,
                            "updated_at": "2026-10-15T00:34:30Z",
                        },
                    ]
                )
            )
            env = {
                "ANTHROPIC_BASE_URL": "http://127.0.0.1:9",
                "NOTION_AGENT_THREADS_STATE": str(state_file),
            }
            out = self.sl.render_statusline(
                {
                    "model": {"display_name": "Sonnet 5.5 (Notion)"},
                    "effort": {"level": "low"},
                    "context_window": {"total_input_tokens": 83000},
                    "cwd": str(pathlib.Path(__file__).parent),
                    "session_id": session_id,
                },
                env=env,
            )
            self.assertIn(f"resume: {session_id}", out)
            self.assertIn("notion: https://app.notion.com/chat?t=bda42e88734b4ce2a0bc75dccb01fd4a", out)

            out_unbound = self.sl.render_statusline(
                {
                    "model": {"display_name": "Sonnet 5.5 (Notion)"},
                    "effort": {"level": "low"},
                    "context_window": {"total_input_tokens": 1000},
                    "cwd": str(pathlib.Path(__file__).parent),
                    "session_id": "unbound-session-id",
                },
                env=env,
            )
            self.assertIn("resume: unbound-session-id", out_unbound)
            self.assertNotIn("notion: https://app.notion.com/chat?t=", out_unbound)

    def _client_key(self, session_id):
        import hashlib
        return hashlib.sha256(f"agent-client:{session_id}".encode("utf-8")).hexdigest()[:32]

    def test_resolve_prefers_newest_updated_at_over_higher_conv_len(self):
        session_id = "rebound-session"
        key = self._client_key(session_id)
        with tempfile.TemporaryDirectory() as td:
            state_file = pathlib.Path(td) / ".agent_threads.state"
            state_file.write_text(json.dumps([
                {"client_key": key, "thread_id": "aaaaaaaa-0000-0000-0000-000000000001",
                 "conv_len": 45, "updated_at": "2026-10-14T23:59:59.123456789Z"},
                {"client_key": key, "thread_id": "bbbbbbbb-0000-0000-0000-000000000002",
                 "conv_len": 6, "updated_at": "2026-10-15T09:00:00-04:00"},
            ]))
            url = self.sl.resolve_notion_thread_url(session_id, state_path=str(state_file))
            self.assertTrue(url.endswith("bbbbbbbb000000000000000000000002"), url)

    def test_parse_updated_at_handles_zones_fractions_and_bad_values(self):
        p = self.sl._parse_updated_at
        self.assertEqual(p("2026-10-15T13:00:00Z"), p("2026-10-15T09:00:00-04:00"))
        self.assertLess(p("2026-10-15T13:00:00.5Z"), p("2026-10-15T13:00:01Z"))
        self.assertLess(p("2026-10-15T13:00:00.123456789Z"), p("2026-10-15T13:00:01Z"))
        self.assertEqual(p(None), 0.0)
        self.assertEqual(p(""), 0.0)
        self.assertEqual(p("not-a-date"), 0.0)

    def test_accounts_dir_expands_tilde_and_ignores_inline_comment(self):
        from unittest import mock
        session_id = "cfg-session"
        key = self._client_key(session_id)
        with tempfile.TemporaryDirectory() as td:
            root = pathlib.Path(td) / "root"
            home = pathlib.Path(td) / "home"
            acc = home / "data" / "accts"
            acc.mkdir(parents=True)
            root.mkdir()
            (root / "config.yaml").write_text("accounts_dir: ~/data/accts   # my comment\n")
            (acc / ".agent_threads.state").write_text(json.dumps([
                {"client_key": key, "thread_id": "cccccccc-0000-0000-0000-000000000003",
                 "conv_len": 1, "updated_at": "2026-10-15T00:00:00Z"},
            ]))
            with mock.patch.object(self.sl, "ROOT", str(root)), \
                    mock.patch.dict(os.environ, {"HOME": str(home)}):
                paths = self.sl._candidate_state_paths(env={})
                self.assertEqual(paths[0], str(acc / ".agent_threads.state"))
                url = self.sl.resolve_notion_thread_url(session_id, env={})
            self.assertTrue(url.endswith("cccccccc000000000000000000000003"), url)


class ClaudeDesktopSetupTest(unittest.TestCase):
    def test_extract_desktop_flags_and_strip_from_claude_argv(self):
        self.assertEqual(cn.extract_desktop_flags(["-p", "hi"]), (False, False))
        self.assertEqual(cn.extract_desktop_flags(["--setup-desktop"]), (True, False))
        self.assertEqual(cn.extract_desktop_flags(["--desktop"]), (True, True))
        rest, notion, dry = cn.extract_launcher_flags(["--setup-desktop", "--desktop", "-p", "hi"])
        self.assertEqual((rest, notion, dry), (["-p", "hi"], None, False))

    def test_desktop_sol_aliases_get_1m_context_window(self):
        for model in ("claude-sol-6-1", "claude-sol-6-1[1m]", "claude-sol-6.1", "claude-sol-6.1[1m]"):
            env = cn.subagent_env(["--model", model], environ={})
            self.assertEqual(env["CLAUDE_CODE_MAX_CONTEXT_TOKENS"], "1000000")
            self.assertEqual(env["CLAUDE_CODE_AUTO_COMPACT_WINDOW"], "1000000")

    def test_setup_claude_desktop_writes_3p_config_and_settings_preserving_existing_keys(self):
        with tempfile.TemporaryDirectory() as td:
            desktop_dir = pathlib.Path(td) / "Claude-3p"
            desktop_dir.mkdir(parents=True)
            config_path = desktop_dir / "config.json"
            config_path.write_text(json.dumps({
                "scale": 0,
                "locale": "en-US",
                "userThemeMode": "system",
                "hasSeenOnboarding": True,
            }), encoding="utf-8")

            user_settings_path = pathlib.Path(td) / "user-claude" / "settings.json"
            user_settings_path.parent.mkdir(parents=True)
            user_settings_path.write_text(json.dumps({
                "outputStyle": "team-attention-span",
                "modelSettings": {
                    "claude-sonnet-5-5": {"effortLevel": "high"},
                    "claude-opus-5-5": {"effortLevel": "medium"},
                    "custom-other": {"effortLevel": "low"},
                },
            }), encoding="utf-8")

            paths = cn.setup_claude_desktop(
                "http://127.0.0.1:18081",
                "test-local-key",
                desktop_dir=desktop_dir,
                user_settings_path=user_settings_path,
            )
            self.assertTrue((desktop_dir / "config.json.bak").is_file())
            cfg = json.loads(paths["config_path"].read_text(encoding="utf-8"))
            self.assertEqual(cfg["locale"], "zh-Hans")
            self.assertTrue(cfg["hasSeenOnboarding"])
            inf = cfg["inferenceConfig"]
            self.assertEqual(inf["provider"], "gateway")
            self.assertEqual(inf["modelSource"], "static")
            self.assertEqual(inf["selectedModel"], "claude-sonnet-5-5")
            self.assertEqual(inf["detectedModels"], [
                "claude-sonnet-5-5",
                "claude-opus-5-5",
                "claude-sol-6-1[1m]",
                "claude-auto",
            ])
            self.assertEqual(inf["customModelIds"], [
                "sonnet-5.5",
                "opus-5.5",
                "gpt-6.1-sol[1m]",
                "auto",
            ])
            self.assertEqual(inf["env"]["ANTHROPIC_BASE_URL"], "http://127.0.0.1:18081")
            self.assertEqual(inf["env"]["ANTHROPIC_AUTH_TOKEN"], "test-local-key")
            self.assertEqual(inf["env"]["ANTHROPIC_API_KEY"], "test-local-key")
            self.assertEqual(inf["env"]["ANTHROPIC_DEFAULT_HAIKU_MODEL"], "claude-sonnet-5-5")

            settings = json.loads(paths["settings_path"].read_text(encoding="utf-8"))
            self.assertEqual(settings["model"], "claude-sonnet-5-5")
            self.assertIn("claude-sol-6-1[1m]", settings["availableModels"])
            self.assertEqual(settings["env"]["ANTHROPIC_BASE_URL"], "http://127.0.0.1:18081")
            self.assertEqual(settings["env"]["CLAUDE_CODE_SUBAGENT_MODEL"], "claude-sonnet-5-5")
            self.assertEqual(settings["env"]["CLAUDE_CODE_AUTO_COMPACT_WINDOW"], "200000")
            for m_key in ("claude-sonnet-5-5", "claude-opus-5-5", "sonnet-5.5", "opus-5.5"):
                self.assertEqual(settings["modelSettings"][m_key]["autoCompactWindow"], 200000)

            user_settings = json.loads(paths["user_settings_path"].read_text(encoding="utf-8"))
            self.assertEqual(user_settings["outputStyle"], "team-attention-span")
            self.assertEqual(user_settings["modelSettings"]["claude-sonnet-5-5"], {
                "effortLevel": "high",
                "autoCompactWindow": 200000,
            })
            self.assertEqual(user_settings["modelSettings"]["claude-opus-5-5"], {
                "effortLevel": "medium",
                "autoCompactWindow": 200000,
            })
            self.assertEqual(user_settings["modelSettings"]["sonnet-5.5"]["autoCompactWindow"], 200000)
            self.assertEqual(user_settings["modelSettings"]["opus-5.5"]["autoCompactWindow"], 200000)
            self.assertEqual(user_settings["modelSettings"]["custom-other"], {"effortLevel": "low"})

            desktop_cfg = json.loads(paths["desktop_config_path"].read_text(encoding="utf-8"))
            self.assertEqual(desktop_cfg["deploymentMode"], "3p")
            self.assertTrue(paths["cred_helper_path"].is_file())
            self.assertIn("test-local-key", paths["cred_helper_path"].read_text(encoding="utf-8"))
            meta = json.loads((desktop_dir / "configLibrary" / "_meta.json").read_text(encoding="utf-8"))
            self.assertRegex(meta["appliedId"], r"^[a-f0-9-]{36}$")
            self.assertEqual(paths["profile_path"].name, f"{meta['appliedId']}.json")
            profile = json.loads(paths["profile_path"].read_text(encoding="utf-8"))
            self.assertEqual(profile["inferenceProvider"], "gateway")
            self.assertEqual(profile["inferenceGatewayBaseUrl"], "http://127.0.0.1:18081")
            self.assertEqual(profile["inferenceCredentialKind"], "helper-script")
            self.assertEqual(profile["inferenceCredentialHelper"], str(paths["cred_helper_path"]))
            self.assertFalse(profile["modelPrefer1mContext"])
            for flag in (
                "scheduledTasksEnabled",
                "builtinBrowserEnabled",
                "autoModeEnabled",
                "keepAwakeEnabled",
                "developerModeEnabled",
                "skillCreationEnabled",
                "toolSearchEnabled",
                "isLocalDevMcpEnabled",
                "mcpPersistentAlwaysAllowEnabled",
            ):
                self.assertTrue(profile[flag], flag)
            tiers = {m.get("anthropicFamilyTier"): m["name"] for m in profile["inferenceModels"] if m.get("anthropicFamilyTier")}
            self.assertEqual(tiers, {
                "sonnet": "claude-sonnet-5-5",
                "haiku": "claude-sonnet-5-5",
                "opus": "claude-opus-5-5",
            })
            by_name = {m.get("labelOverride"): m for m in profile["inferenceModels"]}
            self.assertFalse(by_name["Claude Sonnet 5.5 (Notion)"]["supports1m"])
            self.assertFalse(by_name["Claude Opus 5.5 (Notion)"]["supports1m"])
            self.assertTrue(by_name["GPT-6.1 Sol (Notion)"]["supports1m"])
            self.assertTrue(by_name["GPT-6.1 Sol (Notion)"]["prefer1m"])


if __name__ == "__main__":
    unittest.main()


