import type { RequestEvent } from './types';

/** Search the same request metadata displayed by the log details card. */
export function requestMatchesQuery(e: RequestEvent, query: string): boolean {
  const text = [
    e.path, e.client_ip, e.user_agent, e.model, e.account, e.request_id,
    e.prompt_preset, e.prompt_mode, e.prompt_sha256,
    e.upstream_code, e.upstream_kind,
    ...(e.dropped || []), ...(e.rewritten || []),
  ].filter(Boolean).join(' ').toLowerCase();
  return query.toLowerCase().split(/\s+/).filter(Boolean).every((kw) => text.includes(kw));
}
