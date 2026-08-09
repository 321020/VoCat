import { api } from "./api";

export interface PluginContribution {
  id: string;
  label: string;
  labelZh?: string;
  location: "sidebar" | "proxy";
  after?: string;
  entry: string;
}

export interface InstalledPlugin {
  id: string;
  name: string;
  version: string;
  description?: string;
  author?: string;
  homepage?: string;
  permissions?: string[];
  contributions: PluginContribution[];
  enabled: boolean;
  backendAvailable: boolean;
  backendRunning: boolean;
  backendError?: string;
  installedAt: string;
  sha256: string;
}

export function listPlugins() {
  return api<InstalledPlugin[]>("/extensions");
}

export function pluginAssetURL(plugin: InstalledPlugin, contribution: PluginContribution) {
  return `/plugin-assets/${encodeURIComponent(plugin.id)}/${contribution.entry.split("/").map(encodeURIComponent).join("/")}`;
}
