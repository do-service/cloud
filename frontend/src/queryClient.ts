import { QueryCache, QueryClient } from "@tanstack/react-query";
import { ApiError } from "./api";
import { meQueryKey } from "./auth";

// Shared cache for all server state. Window-focus refetch is off: queries
// that must stay fresh opt into refetchInterval explicitly (see
// BackendStatus), and everything else refetching on every tab switch would
// just be noise against the backend.
export function createQueryClient(retry: number | false = 1): QueryClient {
  const client: QueryClient = new QueryClient({
    // A 401 anywhere means the session ended (expired, logged out in
    // another tab): re-checking "me" makes RequireAuth send the user to
    // the login page instead of every screen showing its own error.
    queryCache: new QueryCache({
      onError: (err, query) => {
        if (err instanceof ApiError && err.status === 401 && query.queryKey[0] !== meQueryKey[0]) {
          void client.invalidateQueries({ queryKey: meQueryKey });
        }
      },
    }),
    defaultOptions: {
      queries: {
        refetchOnWindowFocus: false,
        // An auth failure won't fix itself on retry.
        retry: (failures, err) => retry !== false && !(err instanceof ApiError && err.status === 401) && failures < retry,
      },
    },
  });
  return client;
}

export const queryClient = createQueryClient();
