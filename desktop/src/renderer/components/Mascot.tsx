// GitClaw, gitagent's mascot (MIT, GitAgent Contributors), in one colour per
// conversation. The colour comes from the id, so it is the same everywhere.
import red from "../assets/mascots/gitclaw-red.png";
import orange from "../assets/mascots/gitclaw-orange.png";
import amber from "../assets/mascots/gitclaw-amber.png";
import green from "../assets/mascots/gitclaw-green.png";
import teal from "../assets/mascots/gitclaw-teal.png";
import blue from "../assets/mascots/gitclaw-blue.png";
import indigo from "../assets/mascots/gitclaw-indigo.png";
import purple from "../assets/mascots/gitclaw-purple.png";
import pink from "../assets/mascots/gitclaw-pink.png";

const MASCOTS = [red, orange, amber, green, teal, blue, indigo, purple, pink];

/** Index into the palette for an id (FNV-1a, stable across runs). */
export function mascotIndex(id: string): number {
  let h = 0x811c9dc5;
  for (let i = 0; i < id.length; i++) {
    h ^= id.charCodeAt(i);
    h = Math.imul(h, 0x01000193);
  }
  return (h >>> 0) % MASCOTS.length;
}

// The plush colour of each mascot, for the status ring around it.
const HUES = ["#e2483d", "#f08a3c", "#e9b23a", "#4cb76a", "#3bb8a6", "#4c8bea", "#5b5fd6", "#9b5be0", "#e0559e"];
export const mascotHue = (id: string) => HUES[mascotIndex(id)];

/** An agent's id for its mascot: what people type ("HR crew") becomes "hr-crew". */
export function slugOf(name: string): string {
  let s = name.toLowerCase().replace(/[^a-z0-9]+/g, "-").replace(/^-+|-+$/g, "").slice(0, 50).replace(/-+$/, "");
  if (s && !/^[a-z]/.test(s)) s = "agent-" + s;
  return s;
}

export function Mascot({ id, size = 32 }: { id: string; size?: number }) {
  return <img className="mascot" src={MASCOTS[mascotIndex(id)]} alt="" width={size} height={size} draggable={false} />;
}
