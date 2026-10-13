// Shared editorial content for the HTML brochure and its plain-text companion.
// Recheck README.md, docs/cli-command-surface.md and docs/feature-status.md
// when changing integration or onboarding claims.
export const product = {
  name: 'WitSelf',
  title: 'WitSelf — persistent memory for your AI agents',
  headline: 'Memory that moves with your AI.',
  summary: 'Give your AI agents a persistent identity, facts, and memory across sessions, tools, and machines. Add secure secrets and agent collaboration as you grow.',
  intro: 'Keep the context. Change the tool.',
  introText: 'Decisions, preferences, and project history belong with your agent. WitSelf makes that context available through the CLI and the Model Context Protocol (MCP), wherever the same agent is connected.',
  features: [
    ['Remember what matters', 'Save precise facts and the context behind them. Recall decisions and lessons in the next session.'],
    ['Bring your own AI', 'Use your existing AI tools and model subscriptions. Your AI does the reasoning; WitSelf stores and retrieves its context.'],
    ['Grow into a team', 'Paid plans add encrypted secrets, durable agent messaging, and collaboration. Team adds more isolated workspaces, called realms.'],
  ],
  runtimes: [
    {name: 'Codex', command: 'codex', note: 'MCP + transcript hooks', platforms: 'macOS, Linux, Windows'},
    {name: 'Claude Code', command: 'claude', note: 'MCP + transcript hooks¹', platforms: 'macOS, Linux, Windows'},
    {name: 'Grok Build', command: 'grok', note: 'MCP + transcript hooks¹', platforms: 'macOS, Linux, Windows'},
    {name: 'Cursor', command: 'cursor', note: 'MCP + transcript hooks', platforms: 'macOS, Linux; Windows via WSL'},
    {name: 'DeepSeek Harness', command: 'dsh', note: 'MCP + transcript hooks¹', platforms: 'macOS, Linux, Windows'},
    {name: 'OpenClaw', command: 'openclaw', note: 'Preview · MCP + routing', platforms: 'macOS, Linux, Windows'},
    {name: 'Antigravity', command: 'antigravity', note: 'Preview · MCP + routing', platforms: 'macOS, Linux, Windows'},
    {name: 'GitHub Copilot CLI', command: 'copilot', note: 'Preview · MCP + routing', platforms: 'macOS, Linux, Windows'},
  ],
  runtimeNote: '¹ On native Windows, Claude Code, Grok Build, and DeepSeek Harness install MCP and routing without transcript hooks. Cursor requires WSL on Windows. Preview integrations do not include WitSelf transcript hooks or automatic prompt-context injection. OpenClaw preview requires one configured default agent. Support varies by client and version; an installer is not a guarantee of every model workflow.',
  installers: [
    {name: 'Homebrew · macOS / Linux', command: 'brew install witwave-ai/tap/witself'},
    {name: 'Shell · macOS / Linux', command: 'curl -fsSL https://raw.githubusercontent.com/witwave-ai/witself/main/install.sh | sh'},
    {name: 'PowerShell · Windows x64', command: '[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12\nirm https://raw.githubusercontent.com/witwave-ai/witself/main/install.ps1 | iex'},
  ],
  questions: [
    ['Is WitSelf an AI model?', 'No. WitSelf is the persistent context and identity layer for your agents. Your connected AI client performs the reasoning. Model subscriptions and inference costs stay with that provider.'],
    ['Do I need a web account or dashboard?', 'Create and manage your account with the WitSelf CLI, then use your connected AI integration for agent work. There is no sign-in on this product page. Signup verification and payment setup may open a browser.'],
    ['Can I move between AI tools?', 'Yes. Connect supported tools to the same WitSelf account, realm, and agent to use that agent’s saved facts and memories. Transcript capture and automatic context differ by integration.'],
    ['Can I run it myself?', 'The source and self-hosting documentation are on GitHub. The plans above describe the managed service; self-hosted infrastructure and operations are your responsibility.'],
  ],
};
