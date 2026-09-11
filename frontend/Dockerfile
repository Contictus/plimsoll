# Two stages, like the backend's. The runtime image carries the standalone server and nothing
# else -- no node_modules, no source, no package manager.
FROM node:24-alpine AS build
WORKDIR /app

# The lockfile first, so a dependency change is the only thing that invalidates the install
# layer. Every other edit rebuilds in seconds.
COPY package.json package-lock.json ./
RUN npm ci --no-audit --no-fund

COPY . .
RUN npm run check && npm run build

FROM node:24-alpine AS runtime
WORKDIR /app
ENV NODE_ENV=production
ENV PORT=3000

# Not root. The dashboard renders pages and holds no secret -- the session cookie never
# reaches it, because every request the browser makes for data goes to /api (K27) -- but a
# process that needs no privileges should not have any.
USER node

COPY --from=build --chown=node:node /app/.next/standalone ./
COPY --from=build --chown=node:node /app/.next/static ./.next/static
COPY --from=build --chown=node:node /app/public ./public

EXPOSE 3000
CMD ["node", "server.js"]
