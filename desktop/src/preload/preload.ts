// Preload: the only bridge between the sandboxed renderer and the main
// process. It exposes one object, window.lux, with a fixed set of
// functions. No ipcRenderer, no Node APIs, no generic "invoke".
import { contextBridge, ipcRenderer } from "electron";
import { EVENT_CHANNEL, INVOKE_CHANNEL, type PushedEvent } from "../shared/ipc";
import { buildBridge } from "./bridge";

const bridge = buildBridge(
  (op, args) => ipcRenderer.invoke(INVOKE_CHANNEL, op, args),
  (listener) => {
    const wrapped = (_e: unknown, msg: PushedEvent) => listener(msg);
    ipcRenderer.on(EVENT_CHANNEL, wrapped);
    return () => {
      ipcRenderer.removeListener(EVENT_CHANNEL, wrapped);
    };
  },
);

contextBridge.exposeInMainWorld("lux", bridge);
