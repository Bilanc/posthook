/** @type {import('next').NextConfig} */
const nextConfig = {
  output: "standalone",
  // better-sqlite3 is a native binding; Next must not bundle it.
  serverExternalPackages: ["better-sqlite3"],
};

export default nextConfig;
