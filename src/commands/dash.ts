// `posthook dash` will eventually spawn the locally-installed posthook-dash
// (Next.js) pointed at ~/.posthook/posthook.db, or open the cloud URL when
// authed. Neither path exists yet — the dash repo and the cloud backend are
// both unbuilt. Until then, this stub holds the surface area so users see the
// command in --help and get a clear pointer.
export async function runDash(): Promise<void> {
  console.log("posthook dash — coming soon.");
  console.log("");
  console.log("The web dashboard ships as a separate npm package: posthook-dash.");
  console.log("It's not published yet. Watch the roadmap in README.md for status.");
  console.log("");
  console.log("In the meantime: `posthook metrics` for CLI breakdowns.");
}
