# Directory packages

`chatgpt/` is the package ChatGPT's plugin directory takes as a ZIP: `plugin.json`
(the listing, the five and three review cases, the release notes), `mcp.json`
(this server's URL, signed in with a client metadata document) and `assets/`.
Reviewer access never goes in it; it is entered in the dashboard.

A new upload needs a new `version` in `plugin.json`, and the one here is the
last one uploaded: raise it in the commit the ZIP is built from. The dashboard
keeps a version that has had its review read only, so a resubmission after a
refusal is a new upload too, even when only the reviewer's access changed
(1.0.1, 2026-11-20). Build the ZIP from inside the folder, so that
`plugin.json` is at its root:

```bash
cd plugin/chatgpt && zip -r -X ../chalet-chatgpt-$(jq -r .version plugin.json).zip plugin.json mcp.json assets
```

The review cases expect the review account's sample data (chalet-app's
docs/deploy.md, "Directory reviewers").
