// Links use the fixed provider origin, never a URL supplied in project text.
export function githubRepositoryURL(repository: string): string | undefined {
  const parts = repository.split('/');
  if (parts.length !== 2) return undefined;
  const [owner, name] = parts;
  if (!/^[A-Za-z0-9](?:[A-Za-z0-9-]{0,37}[A-Za-z0-9])?$/u.test(owner)
    || !/^[A-Za-z0-9._-]{1,100}$/u.test(name) || name === '.' || name === '..') return undefined;
  return `https://github.com/${encodeURIComponent(owner)}/${encodeURIComponent(name)}`;
}
