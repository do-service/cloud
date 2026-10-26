// Shared by the scenarios: the bench users from benchgen's tokens file
// and a signed-in GET of one page of a folder.
import http from 'k6/http';
import { SharedArray } from 'k6/data';

export const BASE_URL = (__ENV.BASE_URL || 'http://localhost:8091').replace(/\/$/, '');
// "session" over plain http, "__Host-session" when PUBLIC_BASE_URL is https.
const COOKIE_NAME = __ENV.COOKIE_NAME || 'session';

// One JSON object per line: userId, token, rootId, bigDirId?, dirs[].
// open() resolves a relative path against this file's directory.
export const users = new SharedArray('users', () =>
  open(__ENV.TOKENS || '../tokens.jsonl')
    .split('\n')
    .filter((line) => line.trim() !== '')
    .map((line) => JSON.parse(line)),
);

// listPage fetches one page of dirId's children as user. tags label the
// request in the summary and in thresholds.
export function listPage(user, dirId, { sort = 'name', order = 'asc', limit, cursor, tags = {} } = {}) {
  const q = [`sort=${sort}`, `order=${order}`];
  if (limit) q.push(`limit=${limit}`);
  if (cursor) q.push(`cursor=${encodeURIComponent(cursor)}`);
  return http.get(`${BASE_URL}/api/v1/nodes/${dirId}/children?${q.join('&')}`, {
    headers: { Cookie: `${COOKIE_NAME}=${user.token}` },
    // One name for every folder id, so the summary isn't split per URL.
    tags: { name: 'children', ...tags },
  });
}
