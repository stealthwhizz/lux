import Markdown from "react-markdown";
import { lux } from "../lib/lux";

/**
 * An agent's reply as formatted text. Raw HTML is never rendered, links open
 * in the system browser (https only, checked in the main process), and
 * images from the web are shown as links: the app loads nothing remote.
 */
export function RichText({ text }: { text: string }) {
  return (
    <div className="rich">
      <Markdown
        skipHtml
        components={{
          a: ({ href, children }) => (
            <a href={href} onClick={(e) => { e.preventDefault(); if (href) void lux().openExternal(href); }}>{children}</a>
          ),
          img: ({ src, alt }) => (
            <a href={String(src ?? "")} onClick={(e) => { e.preventDefault(); if (src) void lux().openExternal(String(src)); }}>{alt || "image"}</a>
          ),
        }}
      >
        {text}
      </Markdown>
    </div>
  );
}
