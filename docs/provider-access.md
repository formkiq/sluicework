# Provider sign-in and API keys

Sluicework separates shared API-key inference from personal subscription agents.
Provider authentication does not replace the console's user login.

| Provider | Personal subscription | Shared cluster inference |
| --- | --- | --- |
| ChatGPT | Official SIWC in an OS-user-owned personal agent; eligible deployments/accounts only | OpenAI API key |
| Claude | No generic subscription-token broker; official-client integration would be separate | Anthropic Console API key |
| Gemini | No generic subscription-token broker; official-client integration would be separate | Google AI Studio API key |

Use **AI connections → Personal scheduled tasks** for
[personal-agent setup, approvals and controls](personal-agent.md). Only the local
runtime signs in and holds ChatGPT tokens. The shared console prepares proposals
and displays signed status; it cannot control the personal agent. Existing shared
sign-in/handoff routes return HTTP 410, and the shared broker refuses subscription
inference even if a legacy connection exists. Legacy files are not automatically
transferred or deleted; sign in freshly inside the personal runtime.

OpenAI's [SIWC documentation](https://developers.openai.com/siwc/token-sharing-open-source)
and [terms](https://openai.com/policies/sign-in-with-chatgpt-terms/) require eligible
applications, user-controlled execution and explicit consent for background use.
Paid/remotely hosted applications may require approval. This integration does not
pool subscriptions, serve another user's requests, or switch to API billing.

[Claude's integration policy](https://code.claude.com/docs/en/legal-and-compliance)
and [Gemini's guidance](https://geminicli.com/docs/resources/faq/) distinguish
native-client subscription login from third-party API access. Never copy browser
cookies or native-client tokens into Sluicework's shared provider settings.

## Shared API keys

Administrators use **AI connections → Shared provider API keys → Add API key**.
Follow the provider's **Get API key** link, paste the key, and permit specific model
IDs. API billing/quota is separate from consumer subscriptions. Replacement is
atomic; a blank replacement field preserves the current key. Changing providers
clears the key field to avoid sending it to the wrong vendor.

Networked workers request inference from the shared broker; API keys remain in
its private files. Local models and shared API keys continue to support approved
jobs and the existing team scheduler. The coding gateway remains local-model only.
Credentials are not encrypted at rest by Sluicework. Protect runtime/service
accounts and disks, and reauthorize after restoring backups that exclude secrets.
