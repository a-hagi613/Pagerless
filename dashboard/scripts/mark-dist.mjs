// Vite empties dist before it builds. The repository commits a marker in dist so that `go:embed all:dist` compiles
// in a fresh checkout, before the dashboard has been built; this restores the marker after each build.
import { writeFileSync } from "node:fs";

const marker =
  "This file keeps the embed directory in the repository. The Vite build writes the dashboard's files beside it.\n";

writeFileSync(new URL("../dist/.gitkeep", import.meta.url), marker);
