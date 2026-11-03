export function classifyBrowserFailure(error) {
  if (typeof error?.matcherResult?.name === 'string' || error?.name === 'AssertionError' || error?.message === 'assertion') return 'assertion';
  if (error?.name === 'TimeoutError' || /timeout|timed out|exceeded.*limit/i.test(error?.message || '')) return 'timeout';
  return 'driver';
}
