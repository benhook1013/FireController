import {describe,expect,it} from 'vitest';
import {validReviewSize} from './review-size';

describe('optional PR size context',()=>{
  const base={pull_number:7,files:96,message:'Stored PR size context.'};
  it('accepts either setting without inventing headroom',()=>{
    expect(validReviewSize({...base,suggested_max_pr_files:80,above_suggested:true},7)).toBe(true);
    expect(validReviewSize({...base,max_review_files:100,remaining_files:4},7)).toBe(true);
    expect(validReviewSize({...base,suggested_max_pr_files:80,max_review_files:100,remaining_files:4,above_suggested:true},7)).toBe(true);
    expect(validReviewSize(base,7)).toBe(false);
    expect(validReviewSize({...base,suggested_max_pr_files:80,above_suggested:true,remaining_files:4},7)).toBe(false);
  });
  it('keeps threshold equality, unknowns and historical counts distinct',()=>{
    expect(validReviewSize({...base,files:80,suggested_max_pr_files:80,max_review_files:100,remaining_files:20},7)).toBe(true);
    expect(validReviewSize({...base,files:100,max_review_files:100,remaining_files:0},7)).toBe(true);
    expect(validReviewSize({...base,files:101,max_review_files:100,excess_files:1},7)).toBe(true);
    expect(validReviewSize({pull_number:7,max_review_files:100,message:'File count not yet observed.'},7)).toBe(true);
    expect(validReviewSize({...base,max_review_files:100,remaining_files:4,completed_reviews:6,retained_reviews:8},7)).toBe(true);
    expect(validReviewSize({...base,max_review_files:100,remaining_files:4,completed_reviews:9,retained_reviews:8},7)).toBe(false);
    expect(validReviewSize({...base,max_review_files:100,remaining_files:5},7)).toBe(false);
    expect(validReviewSize({...base,max_review_files:100,remaining_files:4},8)).toBe(false);
  });
  it('represents unavailable optional information without an admission state',()=>{
    expect(validReviewSize({pull_number:7,unavailable:true,message:'PR size information is unavailable.'},7)).toBe(true);
    expect(validReviewSize({pull_number:7,unavailable:true,held:true,message:'Unavailable.'},7)).toBe(false);
  });
});
