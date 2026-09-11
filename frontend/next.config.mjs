/**
 * The dashboard is served under the same origin as the API (K27), so it makes no
 * cross-origin request and there is no CORS configuration anywhere in this project. Every
 * fetch below goes to a path, never to a host.
 *
 * output: "standalone" is what makes the Docker image small enough to be worth building on
 * the single VPS this deploys to.
 */
const nextConfig = {
  output: "standalone",
  reactStrictMode: true,
};

export default nextConfig;
