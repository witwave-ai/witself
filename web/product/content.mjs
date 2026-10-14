// Shared editorial content for the HTML brochure and its plain-text companion.
// Recheck README.md, docs/cli-command-surface.md and docs/feature-status.md
// when changing integration or onboarding claims.
export const product = {
  name: 'WitSelf',
  title: 'WitSelf — persistent memory for your AI agents',
  headline: 'Memory that moves with your AI.',
  summary: 'Give your AI agents identity, facts, and memory across sessions, tools, and machines. Install WitSelf, then manage it through your AI in plain English.',
  management: {
    promise: 'Install once. Manage it by asking your AI.',
    title: 'Your words. Your AI. Your WitSelf.',
    intro: 'Ask the AI you already use to check your plan, explain an update, or help solve a problem. It works through the installed WitSelf CLI. No separate web management dashboard to learn.',
    prompts: [
      ['Stay up to date', 'Check my WitSelf version, explain what changed, and help me update.'],
      ['Understand your plan', 'What’s included in my plan, and what would Professional add?'],
      ['Get help', 'Help me troubleshoot this. If we need support, prepare a ticket for me.'],
    ],
    requirement: 'Use an AI assistant with permission to run local commands and access your WitSelf account. A chat connected only through MCP can use agent tools, but cannot perform these account-management steps.',
    support: 'Ask your AI questions on any plan. Professional and Team also include support tickets; Personal includes public documentation and community resources.',
    exampleTitle: 'From free to Professional',
    example: [
      ['You', 'I’m ready for Professional. What changes?'],
      ['Your AI', 'Professional adds more memory, encrypted secrets, and agent collaboration. It’s {price} USD per month, billed monthly. Shall I start the upgrade?'],
      ['You', 'Yes, go ahead.'],
      ['Your AI', 'Here’s your secure checkout. Once you finish, I’ll check that Professional is active.'],
    ],
    exampleNote: 'Illustrative first paid upgrade. Your AI checks the current plan and price before making a change. You approve the purchase and complete checkout when required.',
  },
  setupPrompt: 'Help me install WitSelf, set up my free account, and connect it to this AI. Walk me through any steps I need to complete myself, then verify that it works.',
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
    ['Do I need a web management dashboard?', 'No. Manage WitSelf through the CLI, directly or by asking your AI to use it. There is no account sign-in on this product page. Signup verification and secure payment setup may open a browser.'],
    ['Can my AI update WitSelf and get support?', 'An AI with local command access can check your installed version, explain release notes, and help run the appropriate installer or package-manager update. It can also help troubleshoot and, on Professional or Team, open and follow support tickets through the CLI. Your AI guides the work; support tickets go to the WitSelf team.'],
    ['Can I move between AI tools?', 'Yes. Connect supported tools to the same WitSelf account, realm, and agent to use that agent’s saved facts and memories. Transcript capture and automatic context differ by integration.'],
    ['Can I run it myself?', 'The source and self-hosting documentation are on GitHub. The plans above describe the managed service; self-hosted infrastructure and operations are your responsibility.'],
  ],
};
