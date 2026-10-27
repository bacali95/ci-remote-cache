import { useEffect, useState } from "react"
import { toast } from "sonner"
import { api, type Token } from "@/lib/api"
import { formatDate } from "@/lib/format"
import { Button } from "@/components/ui/button"
import { Badge } from "@/components/ui/badge"
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card"
import { ConnectSnippet } from "@/components/ConnectSnippet"
import { Input } from "@/components/ui/input"
import { Label } from "@/components/ui/label"
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from "@/components/ui/table"
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle,
  DialogTrigger,
} from "@/components/ui/dialog"
import {
  AlertDialog,
  AlertDialogAction,
  AlertDialogCancel,
  AlertDialogContent,
  AlertDialogDescription,
  AlertDialogFooter,
  AlertDialogHeader,
  AlertDialogTitle,
  AlertDialogTrigger,
} from "@/components/ui/alert-dialog"

export function TokensPage() {
  const [tokens, setTokens] = useState<Token[]>([])
  const [loading, setLoading] = useState(true)
  const [createOpen, setCreateOpen] = useState(false)
  const [name, setName] = useState("")
  const [scope, setScope] = useState<"read" | "write">("write")
  const [freshToken, setFreshToken] = useState<string | null>(null)

  async function load() {
    setLoading(true)
    try {
      const list = await api.get<Token[]>("/tokens")
      setTokens(list ?? [])
    } catch {
      toast.error("Failed to load tokens")
    } finally {
      setLoading(false)
    }
  }

  useEffect(() => {
    load()
  }, [])

  async function createToken() {
    if (!name.trim()) {
      toast.error("Name is required")
      return
    }
    try {
      const created = await api.post<Token & { token: string }>("/tokens", { name, scope })
      setFreshToken(created.token)
      setName("")
      load()
    } catch {
      toast.error("Failed to create token")
    }
  }

  async function revoke(id: number) {
    try {
      await api.del(`/tokens/${id}`)
      toast.success("Token revoked")
      load()
    } catch {
      toast.error("Failed to revoke token")
    }
  }

  return (
    <div className="flex flex-col gap-4">
      <div className="flex items-center justify-between">
        <h1 className="text-xl font-semibold">Access tokens</h1>
        <Dialog
          open={createOpen}
          onOpenChange={(open) => {
            setCreateOpen(open)
            if (!open) setFreshToken(null)
          }}
        >
          <DialogTrigger asChild>
            <Button size="sm">New token</Button>
          </DialogTrigger>
          <DialogContent>
            {freshToken ? (
              <>
                <DialogHeader>
                  <DialogTitle>Token created</DialogTitle>
                  <DialogDescription>
                    Copy this now — it won't be shown again. Store it as a CI secret and expose it
                    to your builds like this:
                  </DialogDescription>
                </DialogHeader>
                <Input
                  readOnly
                  value={freshToken}
                  onFocus={(e) => e.target.select()}
                  className="font-mono text-sm"
                />
                <ConnectSnippet token={freshToken} />
                <DialogFooter>
                  <Button onClick={() => setCreateOpen(false)}>Done</Button>
                </DialogFooter>
              </>
            ) : (
              <>
                <DialogHeader>
                  <DialogTitle>Create access token</DialogTitle>
                  <DialogDescription>
                    Write tokens can upload and download; read tokens can only download.
                  </DialogDescription>
                </DialogHeader>
                <div className="flex flex-col gap-3">
                  <div className="flex flex-col gap-2">
                    <Label htmlFor="token-name">Name</Label>
                    <Input
                      id="token-name"
                      placeholder="e.g. github-actions-main"
                      value={name}
                      onChange={(e) => setName(e.target.value)}
                    />
                  </div>
                  <div className="flex flex-col gap-2">
                    <Label>Scope</Label>
                    <div className="flex gap-2">
                      <Button
                        type="button"
                        variant={scope === "write" ? "default" : "outline"}
                        size="sm"
                        onClick={() => setScope("write")}
                      >
                        write (read + write)
                      </Button>
                      <Button
                        type="button"
                        variant={scope === "read" ? "default" : "outline"}
                        size="sm"
                        onClick={() => setScope("read")}
                      >
                        read only
                      </Button>
                    </div>
                  </div>
                </div>
                <DialogFooter>
                  <Button variant="outline" onClick={() => setCreateOpen(false)}>
                    Cancel
                  </Button>
                  <Button onClick={createToken}>Create</Button>
                </DialogFooter>
              </>
            )}
          </DialogContent>
        </Dialog>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>Connect a repository</CardTitle>
          <CardDescription>
            Set these in your CI job, with a token from below. Nx and Turborepo share the same
            tokens.
          </CardDescription>
        </CardHeader>
        <CardContent>
          <ConnectSnippet />
        </CardContent>
      </Card>

      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>Name</TableHead>
            <TableHead>Scope</TableHead>
            <TableHead>Created</TableHead>
            <TableHead>Last used</TableHead>
            <TableHead className="w-20" />
          </TableRow>
        </TableHeader>
        <TableBody>
          {tokens.map((t) => (
            <TableRow key={t.id} className={t.revokedAt ? "opacity-50" : undefined}>
              <TableCell>{t.name}</TableCell>
              <TableCell className="flex items-center gap-2">
                <Badge variant={t.scope === "write" ? "default" : "secondary"}>{t.scope}</Badge>
                {t.revokedAt && <Badge variant="destructive">revoked</Badge>}
              </TableCell>
              <TableCell>{formatDate(t.createdAt)}</TableCell>
              <TableCell>{t.lastUsedAt ? formatDate(t.lastUsedAt) : "never"}</TableCell>
              <TableCell>
                {t.revokedAt ? (
                  <span className="text-xs text-muted-foreground">
                    Revoked {formatDate(t.revokedAt)}
                  </span>
                ) : (
                  <AlertDialog>
                    <AlertDialogTrigger asChild>
                      <Button variant="ghost" size="sm">
                        Revoke
                      </Button>
                    </AlertDialogTrigger>
                    <AlertDialogContent>
                      <AlertDialogHeader>
                        <AlertDialogTitle>Revoke "{t.name}"?</AlertDialogTitle>
                        <AlertDialogDescription>
                          Any CI job using this token will immediately start getting 401s.
                        </AlertDialogDescription>
                      </AlertDialogHeader>
                      <AlertDialogFooter>
                        <AlertDialogCancel>Cancel</AlertDialogCancel>
                        <AlertDialogAction onClick={() => revoke(t.id)}>Revoke</AlertDialogAction>
                      </AlertDialogFooter>
                    </AlertDialogContent>
                  </AlertDialog>
                )}
              </TableCell>
            </TableRow>
          ))}
          {!loading && tokens.length === 0 && (
            <TableRow>
              <TableCell colSpan={5} className="text-center text-muted-foreground">
                No tokens yet.
              </TableCell>
            </TableRow>
          )}
        </TableBody>
      </Table>
    </div>
  )
}
