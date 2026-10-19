import { useEffect, useState } from 'react';

export function relativeAge(value: string, now = Date.now()): string {
  const stamp = Date.parse(value);
  if (!Number.isFinite(stamp)) return 'Date unknown';
  const minutes = Math.floor((now - stamp) / 60000);
  if (minutes < 0) return 'Future date';
  if (minutes < 1) return 'Just now';
  if (minutes < 60) return `${minutes}m ago`;
  if (minutes < 1440) return `${Math.floor(minutes / 60)}h ${minutes % 60}m ago`;
  return `${Math.floor(minutes / 1440)}d ${Math.floor(minutes % 1440 / 60)}h ago`;
}

export function RelativeTime({ value }: { value: string }) {
  const [now, setNow] = useState(Date.now);
  useEffect(() => { const timer = setInterval(() => setNow(Date.now()), 60000); return () => clearInterval(timer); }, []);
  return <time dateTime={value} title={new Date(value).toLocaleString()}>{relativeAge(value, now)}</time>;
}
