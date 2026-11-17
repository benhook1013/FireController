import { useLayoutEffect, useRef, useState } from 'react';

// Matches the shared desktop pill's 76px width and 5px gap. Mobile retains
// its existing two rows of three; hidden older rounds remain in full history.
export function reviewPillCapacity(width: number, mobile: boolean): number {
  return mobile ? 6 : Math.max(1, Math.floor((width + 5) / 81));
}
export function useReviewPillCapacity() {
  const ref = useRef<HTMLElement>(null);
  const [capacity, setCapacity] = useState<number>();
  useLayoutEffect(() => {
    const element = ref.current;
    if (!element) return;
    const mobile = matchMedia('(max-width:650px)');
    const update = () => {
      const style = getComputedStyle(element);
      const width = element.clientWidth - parseFloat(style.paddingLeft) - parseFloat(style.paddingRight);
      setCapacity(reviewPillCapacity(width, mobile.matches));
    };
    const observer = new ResizeObserver(update);
    observer.observe(element);
    mobile.addEventListener('change', update);
    update();
    return () => { observer.disconnect(); mobile.removeEventListener('change', update); };
  }, []);
  return { ref, capacity: capacity ?? 6, measured: capacity !== undefined };
}
