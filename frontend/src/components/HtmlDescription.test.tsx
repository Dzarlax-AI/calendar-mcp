// @vitest-environment jsdom
import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { HtmlDescription } from "./HtmlDescription";

describe("HtmlDescription", () => {
  it("renders supported formatting and safe links", () => {
    const html = renderToStaticMarkup(<HtmlDescription html={'<p>Hello <strong>team</strong><br><a href="https://example.com">Open</a></p>'} />);
    expect(html).toContain("<strong>team</strong>");
    expect(html).toContain('<a href="https://example.com/" target="_blank" rel="noreferrer">Open</a>');
    expect(html).toContain("<br/>");
  });

  it("does not render active content or unsafe URL schemes", () => {
    const html = renderToStaticMarkup(<HtmlDescription html={'<p onclick="alert(1)">Safe</p><script>alert(1)</script><a href="javascript:alert(1)">Bad link</a>'} />);
    expect(html).toContain("<p>Safe</p>");
    expect(html).not.toContain("onclick");
    expect(html).not.toContain("<script");
    expect(html).not.toContain("javascript:");
    expect(html).toContain("Bad link");
  });
});
