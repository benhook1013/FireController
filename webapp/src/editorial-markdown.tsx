import ReactMarkdown, { type Components } from 'react-markdown';
import { projectMapURL } from './api';
import './editorial.css';

function safeHref(value: string | undefined): value is string {
  if (!value) return false;
  if (value.startsWith('#')) return !/[\u0000-\u0020\u007f]/u.test(value);
  return projectMapURL(value);
}

const components: Components = {
  h1: ({ children }) => <h4>{children}</h4>,
  h2: ({ children }) => <h4>{children}</h4>,
  h3: ({ children }) => <h5>{children}</h5>,
  h4: ({ children }) => <h6>{children}</h6>,
  h5: ({ children }) => <h6>{children}</h6>,
  h6: ({ children }) => <h6>{children}</h6>,
  a: ({ href, children }) => safeHref(href)
    ? <a href={href} {...(href.startsWith('#') ? {} : { target: '_blank', rel: 'noreferrer noopener' })}>{children}</a>
    : <span className="editorial-unsafe-link">{children}</span>,
  img: ({ alt }) => <span className="editorial-image-alt">{alt ? `Image: ${alt}` : 'Image omitted'}</span>,
  ul: ({ children }) => <ul className="editorial-list editorial-list-unordered">{children}</ul>,
  ol: ({ children }) => <ol className="editorial-list editorial-list-ordered">{children}</ol>,
  blockquote: ({ children }) => <blockquote className="editorial-quote">{children}</blockquote>,
  pre: ({ children }) => <pre className="editorial-code-block">{children}</pre>,
  code: ({ children, className }) => <code className={className}>{children}</code>,
};

const compactComponents: Components = {
  ...components,
  h1: ({ children }) => <h5>{children}</h5>,
  h2: ({ children }) => <h5>{children}</h5>,
  h3: ({ children }) => <h6>{children}</h6>,
};

export function EditorialMarkdown({ children, className = '', compact = false }: { children: string; className?: string; compact?: boolean }) {
  return <div className={`editorial-markdown ${className}`.trim()}><ReactMarkdown components={compact ? compactComponents : components}>{children}</ReactMarkdown></div>;
}
