import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { toast } from "sonner"
import { afterEach, describe, expect, it, vi } from "vitest"
import { ConnectSnippet } from "@/components/ConnectSnippet"

vi.mock("sonner", () => ({ toast: { error: vi.fn(), success: vi.fn() } }))

const origin = window.location.origin

describe("ConnectSnippet", () => {
  afterEach(() => {
    vi.clearAllMocks()
    vi.unstubAllGlobals()
  })

  it("shows Nx variables with a placeholder token by default", () => {
    render(<ConnectSnippet />)
    expect(screen.getByLabelText("Environment variables")).toHaveTextContent(
      `NX_SELF_HOSTED_REMOTE_CACHE_SERVER=${origin} NX_SELF_HOSTED_REMOTE_CACHE_ACCESS_TOKEN=<your-token>`,
      { normalizeWhitespace: true }
    )
    expect(screen.getByRole("button", { name: "Nx" })).toHaveAttribute("aria-pressed", "true")
  })

  it("switches to Turborepo variables with the given token", async () => {
    const user = userEvent.setup()
    render(<ConnectSnippet token="crc_secret" />)
    await user.click(screen.getByRole("button", { name: "Turborepo" }))

    const text = screen.getByLabelText("Environment variables").textContent
    expect(text).toContain(`TURBO_API=${origin}`)
    expect(text).toContain("TURBO_TOKEN=crc_secret")
    expect(text).toContain("TURBO_TEAM=")
    expect(screen.getByRole("button", { name: "Turborepo" })).toHaveAttribute(
      "aria-pressed",
      "true"
    )
  })

  it("copies the snippet to the clipboard", async () => {
    const user = userEvent.setup()
    const writeText = vi.spyOn(navigator.clipboard, "writeText").mockResolvedValue()
    render(<ConnectSnippet token="tok" />)

    await user.click(screen.getByRole("button", { name: "Copy" }))

    await waitFor(() => expect(toast.success).toHaveBeenCalledWith("Copied to clipboard"))
    expect(writeText).toHaveBeenCalledWith(
      `NX_SELF_HOSTED_REMOTE_CACHE_SERVER=${origin}\nNX_SELF_HOSTED_REMOTE_CACHE_ACCESS_TOKEN=tok`
    )
  })

  it("tells the user to copy manually when the clipboard is unavailable", async () => {
    const user = userEvent.setup()
    vi.spyOn(navigator.clipboard, "writeText").mockRejectedValue(new Error("denied"))
    render(<ConnectSnippet />)

    await user.click(screen.getByRole("button", { name: "Copy" }))

    await waitFor(() =>
      expect(toast.error).toHaveBeenCalledWith(
        "Couldn't copy — select the text and copy it manually"
      )
    )
  })
})
