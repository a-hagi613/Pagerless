import { createElement } from "react";
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";

import { Markdown } from "./markdown";

function render(source: string): string {
  return renderToStaticMarkup(createElement(Markdown, null, source));
}

describe("Markdown", () => {
  it("renders headings, lists, bold and inline code", () => {
    const html = render("## Observed state\n\n- **Available=False**\n- `inventory-setting` is missing");
    expect(html).toContain("<h3");
    expect(html).toContain("Observed state");
    expect(html).toContain("<ul");
    expect(html).toMatch(/<strong[^>]*>Available=False<\/strong>/);
    expect(html).toContain("<code");
    expect(html).toContain("inventory-setting");
  });

  it("skips raw HTML and refuses unsafe link schemes", () => {
    const html = render("<script>alert(1)</script>\n\n[click](javascript:alert(1)) [ok](https://example.com)");
    expect(html).not.toContain("<script");
    expect(html).not.toContain("javascript:");
    expect(html).toContain('href="https://example.com"');
  });
});
