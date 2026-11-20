import type { ReactNode } from 'react';
import type { IconProps } from '@/components/ui/icons';

// Icons of the brood pages that the shared set lacks (Lucide, ISC), drawn
// the same way: inline paths, stroke in the current colour.

function svg(paths: ReactNode) {
  return function Icon({ size = 20, ...props }: IconProps) {
    return (
      <svg
        xmlns="http://www.w3.org/2000/svg"
        viewBox="0 0 24 24"
        fill="none"
        stroke="currentColor"
        strokeWidth={2}
        strokeLinecap="round"
        strokeLinejoin="round"
        aria-hidden="true"
        focusable="false"
        width={size}
        height={size}
        {...props}
      >
        {paths}
      </svg>
    );
  };
}

export const IconPlay = svg(<polygon points="6 3 20 12 6 21 6 3" />);
export const IconSquare = svg(<rect width="16" height="16" x="4" y="4" rx="2" />);
export const IconPause = svg(
  <>
    <rect x="14" y="4" width="4" height="16" rx="1" />
    <rect x="6" y="4" width="4" height="16" rx="1" />
  </>
);
export const IconTerminal = svg(
  <>
    <path d="m7 11 2-2-2-2" />
    <path d="M11 13h4" />
    <rect width="18" height="18" x="3" y="3" rx="2" ry="2" />
  </>
);
export const IconRotateCcw = svg(
  <>
    <path d="M3 12a9 9 0 1 0 9-9 9.75 9.75 0 0 0-6.74 2.74L3 8" />
    <path d="M3 3v5h5" />
  </>
);
export const IconChevronRight = svg(<path d="m9 18 6-6-6-6" />);
export const IconArrowRight = svg(
  <>
    <path d="M5 12h14" />
    <path d="m12 5 7 7-7 7" />
  </>
);
export const IconPower = svg(
  <>
    <path d="M12 2v10" />
    <path d="M18.4 6.6a9 9 0 1 1-12.77.04" />
  </>
);
export const IconBan = svg(
  <>
    <circle cx="12" cy="12" r="10" />
    <path d="m4.9 4.9 14.2 14.2" />
  </>
);
export const IconHelp = svg(
  <>
    <circle cx="12" cy="12" r="10" />
    <path d="M9.09 9a3 3 0 0 1 5.83 1c0 2-3 3-3 3" />
    <path d="M12 17h.01" />
  </>
);
export const IconBox = svg(
  <>
    <path d="M21 8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16Z" />
    <path d="m3.3 7 8.7 5 8.7-5" />
    <path d="M12 22V12" />
  </>
);
