// The few icons the app needs, drawn on a 20px grid with currentColor.
const base = { width: 18, height: 18, viewBox: "0 0 20 20", fill: "none", stroke: "currentColor", strokeWidth: 1.6, strokeLinecap: "round" as const, strokeLinejoin: "round" as const, "aria-hidden": true };

export const PlusIcon = () => <svg {...base}><path d="M10 4.5v11M4.5 10h11" /></svg>;
export const SearchIcon = () => <svg {...base} width={15} height={15}><circle cx="9" cy="9" r="5.2" /><path d="m13 13 3.2 3.2" /></svg>;
export const SendIcon = () => <svg {...base}><path d="M10 15.5v-11M5.5 9 10 4.5 14.5 9" /></svg>;
export const MoreIcon = () => <svg {...base}><circle cx="5" cy="10" r="0.6" /><circle cx="10" cy="10" r="0.6" /><circle cx="15" cy="10" r="0.6" /></svg>;
export const AttachIcon = () => <svg {...base}><path d="M14.5 9.5l-5 5a3 3 0 0 1-4.2-4.2l6-6a2 2 0 0 1 2.8 2.8l-6 6a1 1 0 0 1-1.4-1.4l5-5" /></svg>;
export const ChevronIcon = () => <svg {...base} width={14} height={14} className="chev"><path d="m6 8 4 4 4-4" /></svg>;
