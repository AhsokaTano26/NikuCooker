// Recreates dist/.gitkeep after a build.
//
// Vite's emptyOutDir wipes dist/ before writing, which takes the tracked
// .gitkeep with it. That file is what makes `//go:embed all:dist` compile on a
// fresh clone with no Node toolchain, and its absence would show up as a
// deleted file in every build's diff.
//
// Restoring it here rather than in the Makefile means a bare `pnpm build` also
// leaves the working tree clean.
import { writeFileSync } from 'node:fs'

writeFileSync(new URL('../dist/.gitkeep', import.meta.url), '')
