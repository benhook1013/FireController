import type { Api } from './api';
import type { Presentation, PresentationSnapshot, ProjectMetadataPage, LiveStatus } from './types';

type MetadataSource = Pick<Api, 'presentation' | 'watchProjectMetadata'>;
type MetadataOwner = {
  lifetime: AbortSignal;
  current(): boolean;
  retry: boolean;
  appearance(value: Presentation): void;
  refresh(page: ProjectMetadataPage | undefined, signal: AbortSignal): Promise<void>;
  status(value: LiveStatus): void;
  failure(error: unknown): void;
};

// One project-bound effect attempt owns both its finite presentation reads and
// its watch. A manual retry starts from a new atomic snapshot, not the original
// session watermark. Worker/inbox changes do not create metadata attempts.
export function startMetadataAttempt(source: MetadataSource, initial: PresentationSnapshot, owner: MetadataOwner): { cancel(): void; done: Promise<void> } {
  const controller = new AbortController();
  const signal = AbortSignal.any([owner.lifetime, controller.signal]);
  const current = () => !signal.aborted && owner.current();
  const done = (async () => {
    if (!current()) return;
    owner.status('connecting');
    const snapshot = owner.retry ? await source.presentation(signal) : initial;
    if (!current()) return;
    let firstSnapshot = true;
    await source.watchProjectMetadata(snapshot, {
      snapshot: async value => {
        if (!current()) return;
        // The connect snapshot preceded content reads. A retry or retention
        // resnapshot must refresh those reads before its watermark is consumed.
        if (owner.retry || !firstSnapshot) await owner.refresh(undefined, signal);
        firstSnapshot = false;
        if (current()) owner.appearance(value.presentation);
      },
      events: async page => {
        if (!current() || page.items.length === 0) return;
        const updated = page.items.some(item => item.event_type === 'project.presentation_changed') ? await source.presentation(signal) : undefined;
        if (!current()) return;
        await owner.refresh(page, signal);
        if (current() && updated) owner.appearance(updated.presentation);
      },
      status: value => { if (current()) owner.status(value); },
    }, signal);
  })().catch(error => {
    if (!current()) return;
    owner.status('closed');
    owner.failure(error);
  });
  return { cancel: () => controller.abort(), done };
}
