import type { Plugin } from 'vite';

export function mockApi(): Plugin {
  return {
    name: 'mock-api',
    configureServer(server) {
      server.middlewares.use((req, res, next) => {
        if (!req.url?.startsWith('/panel/api/')) {
          return next();
        }

        // 允许预检请求
        if (req.method === 'OPTIONS') {
          res.end();
          return;
        }

        const auth = req.headers.authorization;
        // 如果没有鉴权头，返回 401 模拟未登录
        if (!auth) {
          res.statusCode = 401;
          res.end(JSON.stringify({ error: 'unauthorized' }));
          return;
        }

        const path = req.url.slice('/panel/api/'.length).split('?')[0];
        res.setHeader('Content-Type', 'application/json');

        // Overview 接口
        if (req.method === 'GET' && path === 'overview') {
          res.end(JSON.stringify({
            version: "1.0.0-mock",
            go_version: "go1.22.0",
            start_time: new Date(Date.now() - 3600000).toISOString(),
            total: 2,
            healthy: 2,
            cooling: 0,
            disabled: 0,
            paused: 0,
            in_flight_full: 0,
            model_locks: [
              { model: 'gpt-4', realm: 'global', state: 'locked', servable: 0, total: 2, locked: 2, fully_unlock_at: new Date(Date.now() + 3600000).toISOString(), reason: '上游熔断' }
            ],
            proxy_configured: true,
            sticky_sessions: 5,
            accounts: [
              {
                uid: "mock-uid-1",
                nickname: "Mock Account 1",
                realm: "global",
                disabled: false,
                paused: false,
                credits: 50,
                credits_total: 100,
                success_count: 50,
                err_total: 2,
                in_flight: 1,
                token_usage: { request_count: 10, total_tokens: 12345 },
                last_success: new Date().toISOString(),
                rate_limited_models: []
              },
              {
                uid: "mock-uid-2",
                nickname: "Mock Account 2",
                realm: "cn",
                disabled: false,
                paused: false,
                credits: 0,
                credits_total: 100,
                success_count: 120,
                err_total: 0,
                in_flight: 0,
                token_usage: { request_count: 50, total_tokens: 54321 },
                last_success: new Date(Date.now() - 86400000).toISOString(),
                rate_limited_models: [
                  { model: "gpt-4", kind: "model_unavailable", reset_at: new Date(Date.now() + 3600000).toISOString(), reason: "404 model not found" }
                ]
              }
            ]
          }));
          return;
        }

        // Packages 接口（即将过期的积分）
        if (req.method === 'GET' && path === 'packages') {
          res.end(JSON.stringify([
            {
              uid: "mock-uid-1",
              nickname: "Mock Account 1",
              total_available: 50,
              items: [
                { name: "Free Tier", amount: 50, valid_until: new Date(Date.now() + 86400000 * 2).toISOString() }
              ]
            }
          ]));
          return;
        }

        // Tasks 接口
        if (req.method === 'GET' && path === 'tasks') {
          res.end(JSON.stringify([
            { kind: "checkin", last_run: new Date().toISOString(), last_success: true, next_run: new Date(Date.now() + 86400000).toISOString() }
          ]));
          return;
        }

        // 内置指纹表（面板「管理内置指纹表」弹窗；ID 与 scrub.BuiltinRules 同源）
        if (req.method === 'GET' && path === 'scrub/builtin') {
          res.end(JSON.stringify({
            rules: [
              { id: "hdr-billing", kind: "pattern", match: "x-anthropic-billing-header:<值>[;]", note: "#1 图灵/计费 header 键值段整段删除（大小写不敏感）" },
              { id: "hdr-billing-bare", kind: "pattern", match: "x-anthropic-billing-header（裸键名）", replace: "x-anthropic-billing-hdr", note: "#1 残留裸键名缩写为 x-anthropic-billing-hdr" },
              { id: "cc-kv", kind: "pattern", match: "cc_<name>=<值>[;]", note: "#2 尾随 cc_ 键值段整段删除（词边界 + 大小写不敏感）" },
              { id: "identity-claude-code", kind: "literal", match: "You are Claude Code, Anthropic's official CLI for Claude", replace: "You are Claude Code, Anthropic's official CLI tool for Claude", note: "#3 Claude Code 身份句（official CLI → CLI tool）" },
              { id: "identity-claude-agent-sdk", kind: "literal", match: "You are a Claude agent, built on Anthropic's Claude Agent SDK", replace: "You are a Claude agent, built on Anthropic's Claude Agent SDK tool", note: "#3b Claude Agent SDK 身份句（Agent SDK → Agent SDK tool）" },
              { id: "pr-branch", kind: "literal", match: "Main branch (you will usually use this for PRs)", replace: "Default branch (you will usually use this for PRs)", note: "#4 PR 分支注入句（Main branch → Default branch）" },
              { id: "identity-codex", kind: "literal", match: "You are a coding agent running in the Codex CLI, a terminal-based coding assistant.", replace: "You are a coding agent running in the Codex CLI tool, a terminal-based coding assistant.", note: "#5 Codex 身份句（Codex CLI → Codex CLI tool）" },
              { id: "identity-codex-alt", kind: "literal", match: "You are Codex,", replace: "You are the Codex assistant,", note: "#5b Codex 备用身份句（You are Codex, → You are the Codex assistant,）" },
              { id: "feedback-issue", kind: "literal", match: "To give feedback, users should report the issue at https://github.com/anthropics/claude-code/issues", replace: "To provide feedback, users should report the issue at https://github.com/anthropics/claude-code/issues", note: "#6 反馈 issue 句（give → provide feedback）" },
              { id: "err-11128", kind: "literal", match: "11128", replace: "11-128", note: "#7 反探测错误码 11128（上游见到裸串即整单拦截）" }
            ]
          }));
          return;
        }

        // 默认返回成功，支持其他如 balance_all, checkin_all 的 POST 操作
        res.end(JSON.stringify({ status: 'ok' }));
      });
    },
  };
}
