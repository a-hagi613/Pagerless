import { readFileSync, readdirSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import { describe, expect, it } from "vitest";

// The dashboard's images are produced by hack/brand-assets.sh. This test keeps the
// references in index.html and the components honest about the files the build
// actually serves, and catches an asset that shrank, emptied, or changed shape.
const dashboardRoot = resolve(dirname(fileURLToPath(import.meta.url)), "../..");
const publicRoot = join(dashboardRoot, "public");

const expected = new Map([
  ["/favicon.png", { width: 64, height: 64 }],
  ["/pagerless-mascot.png", { width: 256, height: 256 }],
]);

function sourceFiles(directory: string): string[] {
  return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
    const path = join(directory, entry.name);
    if (entry.isDirectory()) {
      return sourceFiles(path);
    }
    // Test files name the expected assets, so they must not count as references.
    return /\.(?:tsx?|css)$/.test(entry.name) && !/\.test\.tsx?$/.test(entry.name) ? [path] : [];
  });
}

function referencedAssets(): string[] {
  const files = [join(dashboardRoot, "index.html"), ...sourceFiles(join(dashboardRoot, "src"))];
  const references = new Set<string>();
  const patterns = [/(?:href|src)=["'](\/[^"']+\.png)["']/g, /url\((["']?)(\/[^"')]+\.png)\1\)/g];
  for (const file of files) {
    const text = readFileSync(file, "utf8");
    for (const pattern of patterns) {
      for (const match of text.matchAll(pattern)) {
        references.add(match[2] ?? match[1]);
      }
    }
  }
  return [...references].sort();
}

interface PngHeader {
  width: number;
  height: number;
  colorType: number;
}

function readPngHeader(path: string): PngHeader {
  const bytes = readFileSync(path);
  expect(bytes.length).toBeGreaterThan(33);
  expect(bytes.subarray(0, 8).toString("hex")).toBe("89504e470d0a1a0a");
  expect(bytes.subarray(12, 16).toString("ascii")).toBe("IHDR");
  return {
    width: bytes.readUInt32BE(16),
    height: bytes.readUInt32BE(20),
    colorType: bytes[25],
  };
}

describe("dashboard brand assets", () => {
  it("references exactly the images the pipeline produces", () => {
    expect(referencedAssets()).toEqual([...expected.keys()].sort());
  });

  it.each([...expected.entries()])("serves %s from public/ at the expected size", (reference, size) => {
    const path = join(publicRoot, reference.slice(1));
    const header = readPngHeader(path);
    expect(header).toMatchObject(size);
    expect(header.colorType).toBe(6);
  });
});
