import { fileURLToPath } from "node:url";

// Static export: `NEXT_EXPORT=1 next build` writes a fully static site to web/out,
// which can be served directly from an nginx web root. Development keeps the /api
// proxy and hot reload because this variable is not set by `next dev`.
const isExport = process.env.NEXT_EXPORT === "1";
// Vercel demo: the entire site uses mocks, with no backend or /api proxy.
const isMock = process.env.NEXT_PUBLIC_MOCK === "1";

/** @type {import('next').NextConfig} */
const nextConfig = {
  // Prevent a parent lockfile from changing root detection and asset paths.
  turbopack: { root: fileURLToPath(new URL(".", import.meta.url)) },
  reactCompiler: true,
  // Allow development assets and HMR from changing LAN IPv4 addresses.
  // Next rejects a bare "*" for security; the segmented wildcard matches IPv4.
  allowedDevOrigins: ["*.*.*.*"],
  compiler: {
    removeConsole: process.env.NODE_ENV === "production",
  },
  ...(isExport
    ? {
        // Static export has no Node runtime, disables image optimization, and
        // writes each route as <route>/index.html.
        output: "export",
        images: { unoptimized: true },
        trailingSlash: true,
      }
    : isMock
      ? {
          // Vercel mock demo has no backend and does not need an /api proxy.
          images: { unoptimized: true },
        }
      : {
          // Development proxies /api/* to the Go backend (:8787 by default,
          // overridable with AUTOPENTEST_API).
          async rewrites() {
            const backend = process.env.AUTOPENTEST_API ?? "http://localhost:8787";
            return [{ source: "/api/:path*", destination: `${backend}/api/:path*` }];
          },
        }),
};

export default nextConfig;
