// Vite handles CSS imports.
declare module "*.css";
declare module "*.png" {
  const src: string;
  export default src;
}
