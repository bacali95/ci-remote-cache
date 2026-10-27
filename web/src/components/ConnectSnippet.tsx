import { useState } from "react"
import { toast } from "sonner"
import { Button } from "@/components/ui/button"

type Client = "nx" | "turbo"

const CLIENTS: { value: Client; label: string }[] = [
  { value: "nx", label: "Nx" },
  { value: "turbo", label: "Turborepo" },
]

function snippet(client: Client, server: string, token: string): string {
  if (client === "nx") {
    return [
      `NX_SELF_HOSTED_REMOTE_CACHE_SERVER=${server}`,
      `NX_SELF_HOSTED_REMOTE_CACHE_ACCESS_TOKEN=${token}`,
    ].join("\n")
  }
  return [
    `TURBO_API=${server}`,
    `TURBO_TOKEN=${token}`,
    "# Required by turbo to enable remote caching; any value works.",
    "TURBO_TEAM=ci",
  ].join("\n")
}

// Environment variables a CI job needs to use this server as its remote
// cache, for each supported client.
export function ConnectSnippet({ token = "<your-token>" }: { token?: string }) {
  const [client, setClient] = useState<Client>("nx")
  const text = snippet(client, window.location.origin, token)

  async function copy() {
    try {
      await navigator.clipboard.writeText(text)
      toast.success("Copied to clipboard")
    } catch {
      toast.error("Couldn't copy — select the text and copy it manually")
    }
  }

  return (
    <div className="flex flex-col gap-2">
      <div className="flex items-center gap-2" role="group" aria-label="Client">
        {CLIENTS.map((c) => (
          <Button
            key={c.value}
            type="button"
            size="sm"
            variant={client === c.value ? "default" : "outline"}
            aria-pressed={client === c.value}
            onClick={() => setClient(c.value)}
          >
            {c.label}
          </Button>
        ))}
        <Button type="button" size="sm" variant="ghost" className="ml-auto" onClick={copy}>
          Copy
        </Button>
      </div>
      <pre
        aria-label="Environment variables"
        className="overflow-x-auto rounded-md bg-muted p-3 font-mono text-xs"
      >
        {text}
      </pre>
    </div>
  )
}
