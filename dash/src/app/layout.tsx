import type { Metadata } from "next";
import Link from "next/link";
import "./globals.css";

export const metadata: Metadata = {
  title: "posthook-dash",
  description: "Local dashboard for posthook AI usage metrics.",
};

export default function RootLayout({ children }: { children: React.ReactNode }) {
  return (
    <html lang="en">
      <body className="min-h-screen">
        <header className="border-b border-[var(--color-border)] bg-[var(--color-bg-elevated)]">
          <div className="mx-auto max-w-7xl px-6 py-4 flex items-center gap-6">
            <Link href="/" className="font-semibold tracking-tight">
              posthook<span className="text-[var(--color-fg-muted)]">/dash</span>
            </Link>
            <nav className="flex gap-4 text-sm">
              <Link href="/" className="hover:text-[var(--color-accent)]">Overview</Link>
              <Link href="/sessions" className="hover:text-[var(--color-accent)]">Sessions</Link>
            </nav>
            <span className="ml-auto text-xs text-[var(--color-fg-muted)] font-mono">
              v0.1.0
            </span>
          </div>
        </header>
        <main className="mx-auto max-w-7xl px-6 py-8">{children}</main>
      </body>
    </html>
  );
}
