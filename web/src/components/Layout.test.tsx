import { render, screen, waitFor } from "@testing-library/react"
import userEvent from "@testing-library/user-event"
import { MemoryRouter, Route, Routes } from "react-router-dom"
import { describe, expect, it, vi } from "vitest"
import { Layout } from "@/components/Layout"

const logout = vi.fn()

vi.mock("@/lib/auth-context", () => ({
  useAuth: () => ({ user: { id: 1, email: "admin@example.com", createdAt: "2024-01-01" }, logout }),
}))

function renderLayout(initialPath = "/") {
  return render(
    <MemoryRouter initialEntries={[initialPath]}>
      <Routes>
        <Route path="/" element={<Layout />}>
          <Route index element={<div>cache page</div>} />
          <Route path="tokens" element={<div>tokens page</div>} />
        </Route>
        <Route path="/login" element={<div>login page</div>} />
      </Routes>
    </MemoryRouter>
  )
}

describe("Layout", () => {
  it("shows the signed-in user's email and renders the active route via Outlet", () => {
    renderLayout("/")
    expect(screen.getByText("admin@example.com")).toBeInTheDocument()
    expect(screen.getByText("cache page")).toBeInTheDocument()
    expect(screen.getByRole("link", { name: "Cache" })).toHaveClass("bg-accent")
    expect(screen.getByRole("link", { name: "Tokens" })).not.toHaveClass("bg-accent")
  })

  it("marks the matching nav link active for a nested route", () => {
    renderLayout("/tokens")
    expect(screen.getByText("tokens page")).toBeInTheDocument()
    expect(screen.getByRole("link", { name: "Tokens" })).toHaveClass("bg-accent")
    expect(screen.getByRole("link", { name: "Cache" })).not.toHaveClass("bg-accent")
  })

  it("navigates home when the title is clicked", async () => {
    const user = userEvent.setup()
    renderLayout("/tokens")

    await user.click(screen.getByRole("link", { name: "CI Remote Cache" }))

    expect(screen.getByText("cache page")).toBeInTheDocument()
  })

  it("logs out and navigates to /login on click", async () => {
    logout.mockResolvedValue(undefined)
    const user = userEvent.setup()
    renderLayout("/")

    await user.click(screen.getByRole("button", { name: "Log out" }))

    expect(logout).toHaveBeenCalled()
    await waitFor(() => expect(screen.getByText("login page")).toBeInTheDocument())
  })
})
