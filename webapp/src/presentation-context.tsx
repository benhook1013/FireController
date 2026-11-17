import { createContext, useContext } from 'react';
import type { Presentation } from './types';
import { heading, type HeadingKey } from './presentation';

export const PresentationContext = createContext<Presentation | undefined>(undefined);
export function usePresentation(): Presentation | undefined { return useContext(PresentationContext); }
export function useProjectHeading(key: HeadingKey): string { return heading(usePresentation(), key); }
