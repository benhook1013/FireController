import { notifyReadingNavigation, scrollToReadingTarget } from './reading-position';

export function scrollToReviewQueueTarget(element: HTMLElement): void {
  const queue = element.closest('#review-queue');
  const rows = queue ? [...queue.querySelectorAll<HTMLElement>('.review-queue-row')] : [];
  const previous = rows[rows.indexOf(element) - 1];
  if (!window.matchMedia('(min-width: 651px)').matches || !previous?.classList.contains('review-row-merged')) {
    scrollToReadingTarget(element);
    return;
  }
  // Merged cards have variable heights. A fixed margin on the current head
  // can clip their title or history; align the actual preceding card instead.
  const edge = Math.max(0, document.querySelector('.mast')?.getBoundingClientRect().bottom ?? 0) + 12;
  notifyReadingNavigation();
  window.scrollBy({ top: previous.getBoundingClientRect().top - edge, left: 0, behavior: 'instant' });
}
