# posthook Devin plugin

A [Devin plugin](https://docs.devin.ai/product-guides/plugins) that calls
`posthook ingest --agent devin` on every tool call and prompt, and flushes to
Bilanc when Devin stops. It assumes the `posthook` binary is already on `PATH`
in the session — install it from your Devin blueprint (see the
[Cloud Agents docs](https://docs.bilanc.co/posthook/cloud-agents)).

Install for your whole Devin organisation from **Settings → Resources →
Plugins → Configuration**:

```json
{
  "requiredPlugins": [
    {
      "source": "git-subdir",
      "url": "https://github.com/Bilanc/posthook.git",
      "path": "plugins/devin",
      "env": {
        "POSTHOOK_CLOUD_TOKEN": "secret:org:POSTHOOK_API_KEY",
        "POSTHOOK_CLOUD_ENABLED": "1",
        "POSTHOOK_CLOUD_ENDPOINT": "https://api.bilanc.co"
      }
    }
  ]
}
```

Drop the `env` block for local-only attribution (no Bilanc workspace).
