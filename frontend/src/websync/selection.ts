import { isArrayOf, isRecord, isString } from "../guards";

// Which cloud folders are synced: include/exclude rules on node ids, as
// the desktop client's selective sync — the nearest rule up the chain
// decides, and without one nothing is. A ticked folder brings its whole
// subtree, what is added to it later included.

// «Доступно мне» isn't a node: this id stands for it, above every share,
// so one tick can take them all.
export const SHARED_ID = "shared";

export type Mode = "include" | "exclude";

export type Rule = {
  id: string;
  mode: Mode;
  // The tree the node is in: the own root's id, a share's id, SHARED_ID.
  root: string;
  // The ids above it, from the top (a share's nodes start with SHARED_ID):
  // what the window needs to show a folder as partly selected without
  // reading the tree.
  chain: string[];
};

// A folder as the window shows it.
export type Place = { id: string; root: string; chain: string[] };

export function isRule(v: unknown): v is Rule {
  return isRecord(v) && isString(v.id) && (v.mode === "include" || v.mode === "exclude") && isString(v.root) && isArrayOf(v.chain, isString);
}

function nearest(rules: Rule[], ids: string[]): boolean {
  for (let i = ids.length - 1; i >= 0; i--) {
    const r = rules.find((x) => x.id === ids[i]);
    if (r) return r.mode === "include";
  }
  return false;
}

export function included(rules: Rule[], place: Place): boolean {
  return nearest(rules, [...place.chain, place.id]);
}

export type Check = "checked" | "partial" | "unchecked";

export function checkState(rules: Rule[], place: Place): Check {
  const on = included(rules, place);
  const mixed = rules.some((r) => r.chain.includes(place.id) && (r.mode === "include") !== on);
  if (mixed) return "partial";
  return on ? "checked" : "unchecked";
}

// setChecked selects (on) or deselects a folder with everything in it:
// the rules below it go, and its own is kept only where it differs from
// what it inherits.
export function setChecked(rules: Rule[], place: Place, on: boolean): Rule[] {
  const rest = rules.filter((r) => r.id !== place.id && !r.chain.includes(place.id));
  if (nearest(rest, place.chain) === on) return rest;
  return [...rest, { id: place.id, mode: on ? "include" : "exclude", root: place.root, chain: place.chain }];
}

// toggle is a click on the checkbox: a partly selected folder becomes
// wholly selected.
export function toggle(rules: Rule[], place: Place): Rule[] {
  return setChecked(rules, place, checkState(rules, place) !== "checked");
}

// wantsTree: whether a tree (an own root or share id) has anything
// selected in it, so its /sync/tree is needed.
export function wantsTree(rules: Rule[], root: string, shared: boolean): boolean {
  if (shared && rules.some((r) => r.id === SHARED_ID && r.mode === "include")) return true;
  return rules.some((r) => r.root === root && r.mode === "include");
}
