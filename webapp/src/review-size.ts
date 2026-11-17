export interface ReviewSize {
  pull_number: number;
  suggested_max_pr_files?: number;
  max_review_files?: number;
  files?: number;
  remaining_files?: number;
  excess_files?: number;
  completed_reviews?: number;
  retained_reviews?: number;
  above_suggested?: boolean;
  unavailable?: boolean;
  message: string;
}

export function validReviewSize(value: unknown, number: number): value is ReviewSize {
  if (!value || typeof value !== 'object' || Array.isArray(value)) return false;
  const v=value as Record<string,unknown>;
  const allowed=['pull_number','suggested_max_pr_files','max_review_files','files','remaining_files','excess_files','completed_reviews','retained_reviews','above_suggested','unavailable','message'];
  if (Object.keys(v).some(key=>!allowed.includes(key)) || v.pull_number!==number || !Number.isSafeInteger(number) || number<1 || number>2147483647 || typeof v.message!=='string' || !v.message.length || v.message.length>1024) return false;
  if (v.unavailable!==undefined && typeof v.unavailable!=='boolean' || v.above_suggested!==undefined && typeof v.above_suggested!=='boolean') return false;
  if (v.unavailable) return Object.keys(v).every(key=>['pull_number','unavailable','message'].includes(key));
  for(const field of ['suggested_max_pr_files','max_review_files','files','remaining_files','excess_files','completed_reviews','retained_reviews']) {
    if(v[field]!==undefined && (!Number.isSafeInteger(v[field]) || Number(v[field])<0)) return false;
  }
  const suggestion=Number(v.suggested_max_pr_files??0),maximum=Number(v.max_review_files??0);
  if(suggestion>100000 || maximum>100000 || suggestion===0 && maximum===0 || Number(v.files??0)>2147483647 || Number(v.completed_reviews??0)>Number(v.retained_reviews??0)) return false;
  const above=v.files!==undefined && suggestion>0 && Number(v.files)>suggestion;
  if(Boolean(v.above_suggested)!==above) return false;
  if(v.files===undefined || maximum===0) return v.remaining_files===undefined && v.excess_files===undefined;
  const remaining=maximum-Number(v.files);
  return remaining>=0 ? v.remaining_files===remaining && v.excess_files===undefined : v.excess_files===-remaining && v.remaining_files===undefined;
}
