import { ActionIcon, Kbd, TextInput } from "@mantine/core";
import { IconSearch } from "@tabler/icons-react";
import { useQuery } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { Link, useLocation, useNavigate, useSearchParams } from "react-router-dom";
import { fetchMe, meQueryKey } from "../auth";
import { t } from "../i18n";

// Typing "/" in these must type it, not jump to the search field.
function isEditable(target: EventTarget | null): boolean {
  return target instanceof HTMLElement && (target.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(target.tagName));
}

export function searchPath(q: string): string {
  return `/search?${new URLSearchParams({ q })}`;
}

// The search field, for a signed-in user: Enter opens /search, "/" focuses
// it from anywhere. On narrow screens the header has no room for it, so an
// icon leads to the page, which has a field of its own.
export default function HeaderSearch() {
  const { pathname } = useLocation();
  const [params] = useSearchParams();
  const navigate = useNavigate();
  const input = useRef<HTMLInputElement>(null);
  const me = useQuery({ queryKey: meQueryKey, queryFn: ({ signal }) => fetchMe(signal), staleTime: Infinity, retry: false });

  // On /search the field shows what was searched for, also after the page's
  // own field or Back changed it; elsewhere it starts empty.
  const current = pathname === "/search" ? (params.get("q") ?? "") : "";
  const [value, setValue] = useState(current);
  const [shown, setShown] = useState(current);
  if (current !== shown) {
    setShown(current);
    setValue(current);
  }

  const signedIn = me.data !== undefined;
  useEffect(() => {
    if (!signedIn) return;
    const onKey = (e: KeyboardEvent) => {
      if (e.key !== "/" || e.ctrlKey || e.metaKey || e.altKey || e.defaultPrevented || isEditable(e.target)) return;
      const el = input.current;
      if (!el) return;
      el.focus();
      // Hidden on a narrow screen, it can't take focus; "/" then stays a key.
      if (document.activeElement === el) e.preventDefault();
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [signedIn]);

  if (!signedIn) return null;
  return (
    <>
      <TextInput
        ref={input}
        visibleFrom="sm"
        w={260}
        size="xs"
        type="search"
        aria-label={t("search.label")}
        placeholder={t("search.placeholder")}
        title={t("search.shortcut")}
        leftSection={<IconSearch size={16} aria-hidden />}
        rightSection={value ? undefined : <Kbd size="xs">/</Kbd>}
        rightSectionPointerEvents="none"
        value={value}
        onChange={(e) => setValue(e.currentTarget.value)}
        onKeyDown={(e) => {
          if (e.key === "Enter") navigate(searchPath(value));
          else if (e.key === "Escape") e.currentTarget.blur();
        }}
      />
      <ActionIcon component={Link} to={current ? searchPath(current) : "/search"} hiddenFrom="sm" variant="subtle" color="gray" aria-label={t("search.open")}>
        <IconSearch size={18} />
      </ActionIcon>
    </>
  );
}
