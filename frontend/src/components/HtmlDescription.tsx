import { createElement, type ReactNode } from "react";

const allowedTags = new Set(["a", "b", "blockquote", "br", "code", "del", "div", "em", "h1", "h2", "h3", "h4", "h5", "h6", "hr", "i", "li", "ol", "p", "pre", "s", "span", "strong", "u", "ul"]);

function safeHref(value: string | null): string | undefined {
  if (!value) return undefined;
  try {
    const url = new URL(value, "https://calendar.local");
    return ["http:", "https:", "mailto:"].includes(url.protocol) ? url.href : undefined;
  } catch {
    return undefined;
  }
}

function renderNode(node: Node, key: string): ReactNode {
  if (node.nodeType === 3) return node.textContent;
  if (node.nodeType !== 1) return null;

  const element = node as HTMLElement;
  const tag = element.tagName.toLowerCase();
  const children = Array.from(element.childNodes, (child, index) => renderNode(child, `${key}-${index}`));
  if (!allowedTags.has(tag)) return children;

  const props: Record<string, unknown> = { key };
  if (tag === "a") {
    const href = safeHref(element.getAttribute("href"));
    if (href) {
      props.href = href;
      props.target = "_blank";
      props.rel = "noreferrer";
    }
  }
  return tag === "br" || tag === "hr" ? createElement(tag, props) : createElement(tag, props, children);
}

// Renders provider-supplied HTML through an explicit allowlist. It deliberately
// does not use dangerouslySetInnerHTML, so scripts, styles, and event handlers
// can never reach the event-details drawer.
export function HtmlDescription({ html }: { html: string }) {
  const document = new DOMParser().parseFromString(html, "text/html");
  return <div className="description-html">{Array.from(document.body.childNodes, (node, index) => renderNode(node, `${index}`))}</div>;
}
