import type { PublicLane } from './public-site-api';
import { DeliveryJobCard, WorkerLaneDisplay } from './delivery-display';

function ApprovedLaneStatus({ value }: { value: string }) {
  const status = value.trim(), normalized = status.toLowerCase();
  const known = ['active', 'blocked', 'parked', 'completed'].includes(normalized);
  return <span className={`badge${known ? ` status-${normalized}` : ''}`}>{known ? normalized[0].toUpperCase() + normalized.slice(1) : status || 'Status not published'}</span>;
}

export function PublicWorkerLanes({ lanes }: { lanes: PublicLane[] }) {
  if (!lanes.length) return <p className="muted">No worker lanes have been published.</p>;
  return <div className="delivery-lanes">{lanes.map(lane => {
    const seen = new Set([lane.headline.trim()]);
    const milestones = lane.milestones.filter(milestone => {
      const text = milestone.trim();
      if (!text || seen.has(text)) return false;
      seen.add(text);
      return true;
    });
    const headline = lane.headline.trim() ? lane.headline : undefined;
    const hasContent = !!headline || milestones.length > 0;
    const status = <ApprovedLaneStatus value={lane.status} />;
    return <WorkerLaneDisplay key={lane.key} label={lane.label} name={lane.label} ariaLabel={`${lane.label} published work`} status={!hasContent ? status : undefined}>
      {hasContent ? <ul className="delivery-job-list"><DeliveryJobCard title={headline} status={status}>
        {milestones.length > 0 && <div className="delivery-job-context"><ul className="public-milestones">{milestones.map(milestone => <li key={milestone}>{milestone}</li>)}</ul></div>}
      </DeliveryJobCard></ul> : <p className="delivery-empty">No approved work details have been published.</p>}
    </WorkerLaneDisplay>;
  })}</div>;
}
