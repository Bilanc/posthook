/** @type {import('next').NextConfig} */
const nextConfig = {
  output: "standalone",
  // The DB driver is node:sqlite, a Node built-in — Next externalizes `node:`
  // imports automatically, so there's nothing native to exclude from bundling.
};

export default nextConfig;
