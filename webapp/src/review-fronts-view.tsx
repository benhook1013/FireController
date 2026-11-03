import { useEffect } from 'react';
import { useQuery } from '@tanstack/react-query';
import { signalFor, useWorkspace } from './workspace';
import './review-fronts.css';

export function useReviewFronts(enabled: boolean) {
  const { session, key, fail } = useWorkspace();
  const query = useQuery({queryKey:[...key,'reviews','current','fronts'],queryFn:({signal})=>session.api.reviewFronts(signalFor(session,signal)),enabled,staleTime:Infinity});
  useEffect(()=>{if(query.error) fail(query.error);},[query.error,fail]);
  return query;
}
