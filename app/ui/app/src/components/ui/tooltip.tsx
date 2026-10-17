import { useEffect, useRef, useState, type ReactNode } from "react";

interface TooltipProps {
  content?: string;
  children: ReactNode;
  className?: string;
  tooltipClassName?: string;
  delayMs?: number;
}

export function Tooltip({
  content,
  children,
  className = "",
  tooltipClassName = "",
  delayMs = 1000,
}: TooltipProps) {
  const [visible, setVisible] = useState(false);
  const [position, setPosition] = useState({ top: 0, left: 0 });
  const timeoutRef = useRef<number | null>(null);
  const anchorRef = useRef<HTMLDivElement>(null);

  const updatePosition = () => {
    const anchor = anchorRef.current;
    if (!anchor) {
      return;
    }

    const rect = anchor.getBoundingClientRect();
    const tooltipWidth = 288;
    const margin = 12;
    const left = Math.min(
      Math.max(rect.left + rect.width / 2, margin + tooltipWidth / 2),
      window.innerWidth - margin - tooltipWidth / 2,
    );

    setPosition({
      top: rect.top - 8,
      left,
    });
  };

  useEffect(() => {
    return () => {
      if (timeoutRef.current) {
        window.clearTimeout(timeoutRef.current);
      }
    };
  }, []);

  const handleMouseEnter = () => {
    if (!content) {
      return;
    }

    if (timeoutRef.current) {
      window.clearTimeout(timeoutRef.current);
    }

    timeoutRef.current = window.setTimeout(() => {
      updatePosition();
      setVisible(true);
    }, delayMs);
  };

  const handleMouseLeave = () => {
    if (timeoutRef.current) {
      window.clearTimeout(timeoutRef.current);
      timeoutRef.current = null;
    }

    setVisible(false);
  };

  const handleFocus = () => {
    if (!content) {
      return;
    }

    updatePosition();
    setVisible(true);
  };

  const handleBlur = () => {
    if (timeoutRef.current) {
      window.clearTimeout(timeoutRef.current);
      timeoutRef.current = null;
    }

    setVisible(false);
  };

  return (
    <div
      ref={anchorRef}
      className={`relative ${className}`}
      onMouseEnter={handleMouseEnter}
      onMouseLeave={handleMouseLeave}
      onFocus={handleFocus}
      onBlur={handleBlur}
    >
      {children}
      {visible && content && (
        <div
          style={{
            top: position.top,
            left: position.left,
          }}
          className={`pointer-events-none fixed z-50 -translate-x-1/2 -translate-y-full rounded-md bg-neutral-800 px-2 py-1 text-[11px] font-medium text-white shadow-lg ${tooltipClassName || "whitespace-nowrap"}`}
        >
          {content}
        </div>
      )}
    </div>
  );
}
