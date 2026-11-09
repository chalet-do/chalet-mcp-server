# Chalet MCP server

Connect your AI assistant to [Chalet](https://chalet.do). This is the server
behind **`https://mcp.chalet.do/mcp`**. Assistants that connect to an MCP
server by URL and sign in with OAuth, such as Claude and ChatGPT, use it to
work in your Chalet as you: projects, to-dos, cards, messages, files, chat,
mail and the CRM.

## Connect

1. The owner of your Chalet account turns on **AI connections** in
   Settings › AI connections.
2. On that page, everyone in the account finds their own link or command for
   their assistant. You can also add `https://mcp.chalet.do/mcp` to your
   assistant as a custom connector.
3. Your assistant sends you to Chalet to sign in. You choose the one account it
   may reach, and its access: **Read only**, or **Read and write**.

To stop, disconnect the app in Settings › Connected apps. It stops working at
once.

An agent member of your account has its own link, on its page in
Settings › AI agents. An assistant that connects through it works as that
agent instead of as you.

## What your assistant can do

The same as [`chalet mcp`](https://github.com/chalet-do/chalet-cli) does on
your own computer: one tool for each part of Chalet, built from what Chalet
opens to agents, so a new feature reaches your assistant without a new version
of this server. A Read only connection gets the reading tools alone.

Some things always wait for you. Trashing or archiving work, changing who may
reach a project, and words your assistant writes for your clients come back as
a link to a page in Chalet, where you read the question and confirm it
yourself. Nothing changes until you click.

## Your data and your tokens

- The server keeps no copy of your data and no token of its own. For each
  connection it trades your assistant's token at Chalet for a Chalet token
  that lasts five minutes at most, and keeps that one in memory until shortly
  before it expires.
- It reaches only the account you chose when you connected, with the access
  you chose, and only while that account's owner keeps AI connections on.
- It sends each change once and never repeats it, and it writes no token to
  its log.
- It runs in Switzerland, beside Chalet itself.

## Development

The tools come from chalet-cli's `mcpserver` package. This repository holds
only the HTTP server around them: the sign-in challenges, the token exchange
and its cache, and the time limits. Go 1.26:

    go vet ./...
    go test -race ./...

The tests run the server against a fake Chalet.
