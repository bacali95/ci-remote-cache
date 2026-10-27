import { render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { toast } from "sonner"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { TokensPage } from "@/pages/TokensPage"
import { api } from "@/lib/api"

vi.mock("@/lib/api", () => ({
  api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), del: vi.fn() },
}))
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }))

const writeToken = {
  id: 1,
  name: "ci-write",
  scope: "write" as const,
  createdAt: "2024-01-01T00:00:00Z",
  lastUsedAt: "2024-03-01T00:00:00Z",
}
const revokedToken = {
  id: 2,
  name: "old-token",
  scope: "read" as const,
  createdAt: "2024-01-01T00:00:00Z",
  revokedAt: "2024-02-01T00:00:00Z",
}

describe("TokensPage", () => {
  beforeEach(() => {
    vi.mocked(api.get).mockResolvedValue([writeToken, revokedToken])
    vi.mocked(api.del).mockResolvedValue(undefined)
  })

  afterEach(() => {
    vi.clearAllMocks()
  })

  it("lists tokens, marking the revoked one distinctly", async () => {
    render(<TokensPage />)

    expect(await screen.findByText("ci-write")).toBeInTheDocument()
    expect(screen.getByText("old-token")).toBeInTheDocument()
    expect(screen.getByText("revoked")).toBeInTheDocument()
    expect(screen.getByText(/Revoked/)).toBeInTheDocument()
    expect(screen.queryByRole("button", { name: "Revoke" })).toBeInTheDocument()
    expect(screen.getByText(new Date(writeToken.lastUsedAt).toLocaleString())).toBeInTheDocument()
  })

  it("shows client setup with a placeholder token on the page", async () => {
    render(<TokensPage />)
    await screen.findByText("ci-write")
    expect(screen.getByText("Connect a repository")).toBeInTheDocument()
    expect(screen.getByLabelText("Environment variables")).toHaveTextContent("<your-token>")
  })

  it("shows an empty state with no tokens", async () => {
    vi.mocked(api.get).mockResolvedValue([])
    render(<TokensPage />)
    expect(await screen.findByText("No tokens yet.")).toBeInTheDocument()
  })

  it("treats a null token list from the API as empty", async () => {
    vi.mocked(api.get).mockResolvedValue(null)
    render(<TokensPage />)
    expect(await screen.findByText("No tokens yet.")).toBeInTheDocument()
  })

  it("requires a name before creating a token", async () => {
    const user = userEvent.setup()
    render(<TokensPage />)
    await screen.findByText("ci-write")

    await user.click(screen.getByRole("button", { name: "New token" }))
    await user.click(screen.getByRole("button", { name: "Create" }))

    expect(toast.error).toHaveBeenCalledWith("Name is required")
    expect(api.post).not.toHaveBeenCalled()
  })

  it("creates a read-scoped token and shows the raw value once", async () => {
    vi.mocked(api.post).mockResolvedValue({
      ...writeToken,
      id: 3,
      scope: "read",
      token: "raw-token-value",
    })
    const user = userEvent.setup()
    render(<TokensPage />)
    await screen.findByText("ci-write")

    await user.click(screen.getByRole("button", { name: "New token" }))
    await user.type(screen.getByLabelText("Name"), "ci-read")
    await user.click(screen.getByRole("button", { name: "read only" }))
    await user.click(screen.getByRole("button", { name: "Create" }))

    await waitFor(() =>
      expect(api.post).toHaveBeenCalledWith("/tokens", { name: "ci-read", scope: "read" })
    )
    expect(await screen.findByText("Token created")).toBeInTheDocument()
    const tokenField = screen.getByDisplayValue("raw-token-value")
    expect(tokenField).toBeInTheDocument()
    const dialog = screen.getByRole("dialog")
    expect(within(dialog).getByLabelText("Environment variables")).toHaveTextContent(
      "NX_SELF_HOSTED_REMOTE_CACHE_ACCESS_TOKEN=raw-token-value"
    )

    await user.click(tokenField)
    expect(tokenField).toHaveFocus()

    await user.click(screen.getByRole("button", { name: "Done" }))
    await waitFor(() => expect(screen.queryByText("Token created")).not.toBeInTheDocument())
  })

  it("clears the shown raw token when the dialog is dismissed via its close button", async () => {
    vi.mocked(api.post).mockResolvedValue({ ...writeToken, id: 3, token: "raw-token-value" })
    const user = userEvent.setup()
    render(<TokensPage />)
    await screen.findByText("ci-write")

    await user.click(screen.getByRole("button", { name: "New token" }))
    await user.type(screen.getByLabelText("Name"), "ci-write-2")
    await user.click(screen.getByRole("button", { name: "Create" }))
    expect(await screen.findByText("Token created")).toBeInTheDocument()

    // The X button Radix renders on DialogContent, as opposed to our own
    // "Done" button — only this one routes through Dialog's onOpenChange.
    await user.click(screen.getByRole("button", { name: "Close" }))
    await waitFor(() => expect(screen.queryByText("Token created")).not.toBeInTheDocument())

    await user.click(screen.getByRole("button", { name: "New token" }))
    expect(await screen.findByText("Create access token")).toBeInTheDocument()
  })

  it("shows a toast when creating a token fails", async () => {
    vi.mocked(api.post).mockRejectedValue(new Error("boom"))
    const user = userEvent.setup()
    render(<TokensPage />)
    await screen.findByText("ci-write")

    await user.click(screen.getByRole("button", { name: "New token" }))
    await user.type(screen.getByLabelText("Name"), "ci-write-2")
    await user.click(screen.getByRole("button", { name: "Create" }))

    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("Failed to create token"))
  })

  it("switching scope back to write after picking read keeps write selected by default", async () => {
    const user = userEvent.setup()
    render(<TokensPage />)
    await screen.findByText("ci-write")

    await user.click(screen.getByRole("button", { name: "New token" }))
    await user.click(screen.getByRole("button", { name: "read only" }))
    await user.click(screen.getByRole("button", { name: "write (read + write)" }))
    await user.type(screen.getByLabelText("Name"), "ci-write-again")
    await user.click(screen.getByRole("button", { name: "Create" }))

    await waitFor(() =>
      expect(api.post).toHaveBeenCalledWith("/tokens", { name: "ci-write-again", scope: "write" })
    )
  })

  it("cancels the create dialog without creating a token", async () => {
    const user = userEvent.setup()
    render(<TokensPage />)
    await screen.findByText("ci-write")

    await user.click(screen.getByRole("button", { name: "New token" }))
    await user.type(screen.getByLabelText("Name"), "abandoned")
    await user.click(screen.getByRole("button", { name: "Cancel" }))

    await waitFor(() => expect(screen.queryByText("Create access token")).not.toBeInTheDocument())
    expect(api.post).not.toHaveBeenCalled()
  })

  it("shows a toast when revoking a token fails", async () => {
    vi.mocked(api.del).mockRejectedValue(new Error("boom"))
    const user = userEvent.setup()
    render(<TokensPage />)
    await screen.findByText("ci-write")

    await user.click(screen.getByRole("button", { name: "Revoke" }))
    const dialog = await screen.findByRole("alertdialog")
    await user.click(within(dialog).getByRole("button", { name: "Revoke" }))

    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("Failed to revoke token"))
  })

  it("revokes a token via its confirm dialog", async () => {
    const user = userEvent.setup()
    render(<TokensPage />)
    await screen.findByText("ci-write")

    await user.click(screen.getByRole("button", { name: "Revoke" }))
    const dialog = await screen.findByRole("alertdialog")
    expect(within(dialog).getByText('Revoke "ci-write"?')).toBeInTheDocument()
    await user.click(within(dialog).getByRole("button", { name: "Revoke" }))

    await waitFor(() => expect(api.del).toHaveBeenCalledWith("/tokens/1"))
    expect(toast.success).toHaveBeenCalledWith("Token revoked")
  })

  it("shows a toast when loading tokens fails", async () => {
    vi.mocked(api.get).mockRejectedValue(new Error("boom"))
    render(<TokensPage />)
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("Failed to load tokens"))
  })
})
