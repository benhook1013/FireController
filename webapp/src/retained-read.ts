import { useLayoutEffect, useRef } from 'react';

export function retainedRead<T>(scope: string, current: T | undefined, previous?: { scope: string; value: T }, enabled = true): T | undefined {
  return enabled ? current ?? (previous?.scope === scope ? previous.value : undefined) : undefined;
}

// Keep verified content on screen while its next version loads. The caller's
// scope includes the authenticated connection and resource, never just a label.
export function useRetainedRead<T>(scope: string, current: T | undefined, enabled = true): T | undefined {
  const previous = useRef<{ scope: string; value: T } | undefined>(undefined);
  const value = retainedRead(scope, current, previous.current, enabled);
  useLayoutEffect(() => {
    previous.current = value === undefined ? undefined : { scope, value };
  }, [scope, value]);
  return value;
}
