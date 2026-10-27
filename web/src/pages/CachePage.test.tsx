import { render, screen, waitFor, within } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { toast } from "sonner"
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest"
import { CachePage } from "@/pages/CachePage"
import { api } from "@/lib/api"

vi.mock("@/lib/api", () => ({
  api: { get: vi.fn(), post: vi.fn(), put: vi.fn(), del: vi.fn() },
}))
vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }))

const entryA = {
  key: "nx/hashaaa",
  protocol: "nx",
  hash: "hashaaa",
  size: 2048,
  modTime: "2024-01-01T00:00:00Z",
  readCount: 3,
}
const entryB = {
  key: "turbo/hashbbb",
  protocol: "turbo",
  hash: "hashbbb",
  size: 512,
  modTime: "2024-01-02T00:00:00Z",
  readCount: 0,
  lastReadAt: "2024-01-03T00:00:00Z",
}

describe("CachePage", () => {
  beforeEach(() => {
    vi.mocked(api.get).mockResolvedValue({ entries: [entryA, entryB] })
    vi.mocked(api.del).mockResolvedValue(undefined)
    vi.mocked(api.post).mockResolvedValue({ deleted: 1 })
  })

  afterEach(() => {
    vi.clearAllMocks()
  })

  it("loads and lists cache entries with formatted size and dates", async () => {
    render(<CachePage />)

    expect(await screen.findByText("hashaaa")).toBeInTheDocument()
    expect(screen.getByText("hashbbb")).toBeInTheDocument()
    expect(screen.getByText("2.0 KB")).toBeInTheDocument()
    expect(screen.getByText("3")).toBeInTheDocument()
    expect(screen.getByText("never")).toBeInTheDocument()
    expect(api.get).toHaveBeenCalledWith("/cache?")
  })

  it("shows an empty state when there are no entries", async () => {
    vi.mocked(api.get).mockResolvedValue({ entries: [] })
    render(<CachePage />)
    expect(await screen.findByText("No cache entries.")).toBeInTheDocument()
  })

  it("treats a null entries list from the API as empty", async () => {
    vi.mocked(api.get).mockResolvedValue({ entries: null })
    render(<CachePage />)
    expect(await screen.findByText("No cache entries.")).toBeInTheDocument()
  })

  it("shows a toast and stays empty when loading fails", async () => {
    vi.mocked(api.get).mockRejectedValue(new Error("network error"))
    render(<CachePage />)
    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("Failed to load cache entries"))
    expect(screen.getByText("No cache entries.")).toBeInTheDocument()
  })

  it("deletes a single entry via its row's confirm dialog", async () => {
    const user = userEvent.setup()
    render(<CachePage />)
    await screen.findByText("hashaaa")

    const row = screen.getByText("hashaaa").closest("tr")!
    await user.click(within(row).getByRole("button", { name: "Delete" }))

    const dialog = await screen.findByRole("alertdialog")
    expect(within(dialog).getByText("Delete this entry?")).toBeInTheDocument()
    await user.click(within(dialog).getByRole("button", { name: "Delete" }))

    await waitFor(() => expect(api.del).toHaveBeenCalledWith("/cache/nx%2Fhashaaa"))
    expect(toast.success).toHaveBeenCalledWith("Deleted nx/hashaaa")
    await waitFor(() => expect(screen.queryByText("hashaaa")).not.toBeInTheDocument())
  })

  it("shows a toast when deleting a single entry fails", async () => {
    vi.mocked(api.del).mockRejectedValue(new Error("boom"))
    const user = userEvent.setup()
    render(<CachePage />)
    await screen.findByText("hashaaa")

    const row = screen.getByText("hashaaa").closest("tr")!
    await user.click(within(row).getByRole("button", { name: "Delete" }))
    const dialog = await screen.findByRole("alertdialog")
    await user.click(within(dialog).getByRole("button", { name: "Delete" }))

    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("Failed to delete nx/hashaaa"))
    expect(screen.getByText("hashaaa")).toBeInTheDocument()
  })

  it("shows a toast when bulk delete fails", async () => {
    vi.mocked(api.post).mockRejectedValue(new Error("boom"))
    const user = userEvent.setup()
    render(<CachePage />)
    await screen.findByText("hashaaa")

    const rowA = screen.getByText("hashaaa").closest("tr")!
    await user.click(within(rowA).getByRole("checkbox"))
    await user.click(screen.getByRole("button", { name: /Delete selected/ }))
    const dialog = await screen.findByRole("alertdialog")
    await user.click(within(dialog).getByRole("button", { name: "Delete" }))

    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("Bulk delete failed"))
  })

  it("shows a toast when prune fails", async () => {
    vi.mocked(api.post).mockRejectedValue(new Error("boom"))
    const user = userEvent.setup()
    render(<CachePage />)
    await screen.findByText("hashaaa")

    await user.click(screen.getByRole("button", { name: "Prune by age" }))
    await user.click(screen.getByRole("button", { name: "Prune" }))

    await waitFor(() => expect(toast.error).toHaveBeenCalledWith("Prune failed"))
  })

  it("cancels the prune dialog without pruning", async () => {
    const user = userEvent.setup()
    render(<CachePage />)
    await screen.findByText("hashaaa")

    await user.click(screen.getByRole("button", { name: "Prune by age" }))
    await user.click(screen.getByRole("button", { name: "Cancel" }))

    await waitFor(() =>
      expect(screen.queryByText("Prune old cache entries")).not.toBeInTheDocument()
    )
    expect(api.post).not.toHaveBeenCalled()
  })

  it("reloads entries when Refresh is clicked", async () => {
    const user = userEvent.setup()
    render(<CachePage />)
    await screen.findByText("hashaaa")

    await user.click(screen.getByRole("button", { name: "Refresh" }))
    await waitFor(() => expect(api.get).toHaveBeenCalledTimes(2))
  })

  it("toggles a row's selection on and off", async () => {
    const user = userEvent.setup()
    render(<CachePage />)
    await screen.findByText("hashaaa")

    const deleteSelectedButton = () => screen.getByRole("button", { name: /Delete selected/ })
    const checkbox = within(screen.getByText("hashaaa").closest("tr")!).getByRole("checkbox")

    expect(deleteSelectedButton()).toBeDisabled()
    await user.click(checkbox)
    expect(deleteSelectedButton()).toHaveTextContent("Delete selected (1)")
    await user.click(checkbox)
    expect(deleteSelectedButton()).toHaveTextContent("Delete selected (0)")
    expect(deleteSelectedButton()).toBeDisabled()
  })

  it("bulk-deletes a single selected entry with singular wording", async () => {
    const user = userEvent.setup()
    render(<CachePage />)
    await screen.findByText("hashaaa")

    await user.click(within(screen.getByText("hashaaa").closest("tr")!).getByRole("checkbox"))
    await user.click(screen.getByRole("button", { name: /Delete selected/ }))
    const dialog = await screen.findByRole("alertdialog")
    await user.click(within(dialog).getByRole("button", { name: "Delete" }))

    await waitFor(() =>
      expect(api.post).toHaveBeenCalledWith("/cache/bulk-delete", { keys: ["nx/hashaaa"] })
    )
    expect(toast.success).toHaveBeenCalledWith("Deleted 1 entry")
  })

  it("selects multiple entries and bulk-deletes them", async () => {
    vi.mocked(api.post).mockResolvedValue({ deleted: 2 })
    const user = userEvent.setup()
    render(<CachePage />)
    await screen.findByText("hashaaa")

    const deleteSelectedButton = () => screen.getByRole("button", { name: /Delete selected/ })
    await user.click(within(screen.getByText("hashaaa").closest("tr")!).getByRole("checkbox"))
    await user.click(within(screen.getByText("hashbbb").closest("tr")!).getByRole("checkbox"))
    expect(deleteSelectedButton()).toHaveTextContent("Delete selected (2)")

    await user.click(deleteSelectedButton())
    const dialog = await screen.findByRole("alertdialog")
    await user.click(within(dialog).getByRole("button", { name: "Delete" }))

    await waitFor(() =>
      expect(api.post).toHaveBeenCalledWith("/cache/bulk-delete", {
        keys: ["nx/hashaaa", "turbo/hashbbb"],
      })
    )
    expect(toast.success).toHaveBeenCalledWith("Deleted 2 entries")
  })

  it("selects all entries via the header checkbox", async () => {
    const user = userEvent.setup()
    render(<CachePage />)
    await screen.findByText("hashaaa")

    await user.click(screen.getByRole("checkbox", { name: "Select all" }))
    expect(screen.getByRole("button", { name: "Delete selected (2)" })).toBeInTheDocument()

    await user.click(screen.getByRole("checkbox", { name: "Select all" }))
    expect(screen.getByRole("button", { name: "Delete selected (0)" })).toBeInTheDocument()
  })

  it("validates the prune form before submitting", async () => {
    const user = userEvent.setup()
    render(<CachePage />)
    await screen.findByText("hashaaa")

    await user.click(screen.getByRole("button", { name: "Prune by age" }))
    const daysInput = screen.getByLabelText("Older than (days)")
    await user.clear(daysInput)
    await user.type(daysInput, "0")
    await user.click(screen.getByRole("button", { name: "Prune" }))

    expect(toast.error).toHaveBeenCalledWith("Enter a positive number of days")
    expect(api.post).not.toHaveBeenCalled()
  })

  it("prunes entries older than N days and closes the dialog", async () => {
    const user = userEvent.setup()
    render(<CachePage />)
    await screen.findByText("hashaaa")

    await user.click(screen.getByRole("button", { name: "Prune by age" }))
    const daysInput = screen.getByLabelText("Older than (days)")
    await user.clear(daysInput)
    await user.type(daysInput, "7")
    await user.click(screen.getByRole("button", { name: "Prune" }))

    await waitFor(() => expect(api.post).toHaveBeenCalledWith("/cache/prune", { olderThanDays: 7 }))
    expect(toast.success).toHaveBeenCalledWith("Pruned 1 entry older than 7 days")
    await waitFor(() =>
      expect(screen.queryByText("Prune old cache entries")).not.toBeInTheDocument()
    )
  })

  it("pluralizes the prune success message when more than one entry is deleted", async () => {
    vi.mocked(api.post).mockResolvedValue({ deleted: 3 })
    const user = userEvent.setup()
    render(<CachePage />)
    await screen.findByText("hashaaa")

    await user.click(screen.getByRole("button", { name: "Prune by age" }))
    await user.click(screen.getByRole("button", { name: "Prune" }))

    await waitFor(() =>
      expect(toast.success).toHaveBeenCalledWith("Pruned 3 entries older than 30 days")
    )
  })

  it("loads the next page and appends entries when Load more is clicked", async () => {
    vi.mocked(api.get).mockResolvedValueOnce({ entries: [entryA], nextCursor: "cursor-2" })
    const user = userEvent.setup()
    render(<CachePage />)
    await screen.findByText("hashaaa")
    expect(screen.queryByText("hashbbb")).not.toBeInTheDocument()

    vi.mocked(api.get).mockResolvedValueOnce({ entries: [entryB] })
    await user.click(screen.getByRole("button", { name: "Load more" }))

    expect(await screen.findByText("hashbbb")).toBeInTheDocument()
    expect(screen.getByText("hashaaa")).toBeInTheDocument()
    expect(api.get).toHaveBeenLastCalledWith("/cache?cursor=cursor-2")
  })

  it("treats a null entries list on a subsequent page as appending nothing", async () => {
    vi.mocked(api.get).mockResolvedValueOnce({ entries: [entryA], nextCursor: "cursor-2" })
    const user = userEvent.setup()
    render(<CachePage />)
    await screen.findByText("hashaaa")

    vi.mocked(api.get).mockResolvedValueOnce({ entries: null })
    await user.click(screen.getByRole("button", { name: "Load more" }))

    await waitFor(() => expect(api.get).toHaveBeenCalledTimes(2))
    expect(screen.getByText("hashaaa")).toBeInTheDocument()
    expect(screen.queryByText("hashbbb")).not.toBeInTheDocument()
  })

  it("labels each entry's client, marking legacy entries", async () => {
    const legacy = { ...entryA, key: "oldhash", hash: "oldhash", legacy: true }
    const unknown = { ...entryA, key: "bazel/xyz", protocol: "bazel", hash: "xyz" }
    vi.mocked(api.get).mockResolvedValue({ entries: [entryA, entryB, legacy, unknown] })
    render(<CachePage />)

    const row = (hash: string) => screen.getByText(hash).closest("tr")!
    await screen.findByText("hashaaa")
    expect(within(row("hashaaa")).getByText("Nx")).toBeInTheDocument()
    expect(within(row("hashbbb")).getByText("Turborepo")).toBeInTheDocument()
    expect(within(row("oldhash")).getByText("legacy")).toBeInTheDocument()
    expect(within(row("hashaaa")).queryByText("legacy")).not.toBeInTheDocument()
    expect(within(row("xyz")).getByText("bazel")).toBeInTheDocument()
  })

  it("filters by client and clears the selection when the filter changes", async () => {
    const user = userEvent.setup()
    render(<CachePage />)
    await screen.findByText("hashaaa")
    const filters = screen.getByRole("group", { name: "Filter by client" })
    expect(within(filters).getByRole("button", { name: "All" })).toHaveAttribute(
      "aria-pressed",
      "true"
    )

    await user.click(within(screen.getByText("hashaaa").closest("tr")!).getByRole("checkbox"))
    expect(screen.getByRole("button", { name: "Delete selected (1)" })).toBeEnabled()

    vi.mocked(api.get).mockResolvedValueOnce({ entries: [entryB] })
    await user.click(within(filters).getByRole("button", { name: "Turborepo" }))

    await waitFor(() => expect(api.get).toHaveBeenLastCalledWith("/cache?protocol=turbo"))
    await waitFor(() => expect(screen.queryByText("hashaaa")).not.toBeInTheDocument())
    expect(within(filters).getByRole("button", { name: "Turborepo" })).toHaveAttribute(
      "aria-pressed",
      "true"
    )
    expect(screen.getByRole("button", { name: "Delete selected (0)" })).toBeDisabled()
  })
})
