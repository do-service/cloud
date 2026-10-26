import { useQuery } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { Navigate, useLocation } from "react-router-dom";
import { ApiError } from "../api";
import { fetchMe, meQueryKey } from "../auth";
import { ErrorState, PageLoader } from "./States";

// Renders children for a signed-in user; anyone else goes to /login, which
// brings them back here afterwards.
export default function RequireAuth({ children }: { children: ReactNode }) {
  const location = useLocation();
  const me = useQuery({ queryKey: meQueryKey, queryFn: ({ signal }) => fetchMe(signal), staleTime: Infinity });

  if (me.isError && me.error instanceof ApiError && me.error.status === 401) {
    const next = location.pathname + location.search;
    return <Navigate to={`/login?next=${encodeURIComponent(next)}`} replace />;
  }
  if (me.isError) return <ErrorState error={me.error} onRetry={() => void me.refetch()} />;
  if (me.isPending) return <PageLoader />;
  return children;
}
