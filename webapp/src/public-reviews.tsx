import { publicURL, type PublicReview, type PublicRound } from './public-site-api';
import { RelativeTime } from './relative-time';
import { ReviewLaneDisplay, ReviewRoundPillDisplay, ReviewRowDisplay } from './review-display';

function publishedPullURL(review: PublicReview): string | undefined {
  if (!publicURL(review.url)) return undefined;
  const value = new URL(review.url);
  return value.protocol === 'https:' && value.host === 'github.com' && !value.search && !value.hash && /^\/[A-Za-z0-9_.-]+\/[A-Za-z0-9_.-]+\/pull\/\d+$/u.test(value.pathname) && value.pathname.endsWith(`/pull/${review.number}`) ? review.url : undefined;
}
function PublicRoundPill({ round, channel, url }: { round: PublicRound; channel: string; url?: string }) {
  const duration = round.duration_milliseconds === null ? 'Duration unknown' : `${(round.duration_milliseconds / 1000).toLocaleString()} seconds`;
  const description = `${round.found}/${round.accepted}/${round.routed} (found / accepted / routed). ${round.observed_at ? `Observed ${new Date(round.observed_at).toLocaleString()} (${round.observed_at})` : 'Time unknown'}. ${duration}.${round.complete ? '' : ' Incomplete coverage.'}`;
  return <ReviewRoundPillDisplay found={round.found} accepted={round.accepted} routed={round.routed} className={round.complete ? 'review-pill-completed' : ''} zeroAccepted={round.complete && round.accepted === 0} title={description} label={`Published ${channel} round: ${description}`} wrap={url ? content => <a {...content} href={url} target="_blank" rel="noopener noreferrer" /> : undefined}>{round.observed_at ? <RelativeTime value={round.observed_at} /> : <span className="review-pill-age">Time unknown</span>}{!round.complete && <span className="review-pill-state">Incomplete coverage</span>}</ReviewRoundPillDisplay>;
}
function PublicReviewLane({ title, rounds, hasMore, url, browseHistory }: { title: string; rounds: PublicRound[]; hasMore: boolean; url?: string; browseHistory?: () => void }) {
  // The approved projection owns chronology (including undated/tied records).
  // Keep its order; observation clocks and opaque keys are not a browser sort.
  const recent = rounds.slice(-6), outside = hasMore || rounds.length > recent.length;
  const metadata = `${rounds.length} published ${rounds.length === 1 ? 'round' : 'rounds'}`;
  return <ReviewLaneDisplay title={title} label={`${title} review rounds`} metadata={metadata} metadataTitle="Published historical rounds; not current-head coverage">
    {outside && <p className="notice">More published {title} rounds are outside this view. <a href="#published-history" onClick={browseHistory}>Browse retained published revisions</a>; these snapshots may not contain every round.</p>}
    {recent.length ? <div className="review-round-pills review-recent-rounds">{recent.map(round => <PublicRoundPill key={round.key} round={round} channel={title} url={url} />)}</div> : <p className="review-lane-empty">{hasMore ? 'No finalized rounds fit in this view.' : 'No finalized rounds published.'}</p>}
  </ReviewLaneDisplay>;
}

// No private workspace provider or read is needed to display this allowlisted DTO.
export function PublicReviews({ reviews, browseHistory }: { reviews: PublicReview[]; browseHistory?: () => void }) {
  if (!reviews.length) return <p className="muted">No pull requests have been selected for publication.</p>;
  return <section className="panel review-queue-panel"><ul className="review-queue">{reviews.map((review, index) => {
    const position = String(index + 1).padStart(2, '0'), url = publishedPullURL(review);
    const lifecycle = review.lifecycle === 'unknown' ? 'Lifecycle unknown' : review.lifecycle.charAt(0).toUpperCase() + review.lifecycle.slice(1);
    return <ReviewRowDisplay key={review.number} lifecycle={review.lifecycle} position={position} positionLabel={`Published order ${position} on this page`} number={review.number} title={review.title} url={url} facts={<div className="review-pull-facts"><span className={`badge review-state-${review.lifecycle}`}>{lifecycle}</span></div>}>
      <section className="review-history" aria-label={`PR #${review.number} published review rounds`}><div className="review-lanes">
        <PublicReviewLane title="Hosted" rounds={review.hosted} hasMore={review.hosted_has_more ?? false} url={url} browseHistory={browseHistory} />
        <PublicReviewLane title="CLI" rounds={review.cli} hasMore={review.cli_has_more ?? false} url={url} browseHistory={browseHistory} />
      </div></section>
    </ReviewRowDisplay>;
  })}</ul></section>;
}
