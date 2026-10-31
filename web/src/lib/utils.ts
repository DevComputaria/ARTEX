import { type ClassValue, clsx } from "clsx";
import { twMerge } from "tailwind-merge";

export function cn(...inputs: ClassValue[]) {
  return twMerge(clsx(inputs));
}

// The onInteractOutside of a Drawer/Sheet/Dialog turns off the decision aid.
//
// Background: Radix layers in a drawer (Select pulldown, DropdownMenu, Popover, etc.) portal
// outside the drawer. When you mask/collapse the drawer while the layer is open, this time
// pointerdown will be processed by both Select and Sheet DismissableLayers at the same time; Select
// is closed first and is a discrete event, React will flush synchronously, so that when it's the
// turn of the Sheet processor, the data-state of the layer has already been turned to closed —
// naturally unreliable (tested).
//
// The right thing to do: Radix's pointerdown listens at the bubble stage; we record "is there a
// layer open at this time" at the capture stage (before it), and onInteractOutside reads this
// record value to decide whether to release the line.
function isRadixOverlayOpenNow(): boolean {
  if (typeof document === "undefined") return false;
  return !!document.querySelector(
    [
      "[data-slot='select-trigger'][data-state='open']",
      "[data-slot='select-content'][data-state='open']",
      "[role='listbox'][data-state='open']",
      "[data-radix-popper-content-wrapper]",
      "[aria-expanded='true'][data-state='open']",
    ].join(","),
  );
}

let overlayOpenAtLastPointerDown = false;
if (typeof document !== "undefined") {
  document.addEventListener(
    "pointerdown",
    () => {
      overlayOpenAtLastPointerDown = isRadixOverlayOpenNow();
    },
    true, // capture: captures the pointerdown processor's record before the Radix bubble stage
  );
}

// radixOverlayWasOpenAtPointerDown Returns "If there was a Radix overlay open when the last
// pointerdown occurred." The drawer/dialog box is accordingly: Overlay masked when open → Only
// close overlay, not itself.
export function radixOverlayWasOpenAtPointerDown(): boolean {
  return overlayOpenAtLastPointerDown;
}

// Background: Navigator.clipboard is only available in a secure context (HTTPS / localhost); when
// accessed via IP + HTTP, it is undefined and is degraded to execCommand("copy").
export async function copyText(text: string): Promise<boolean> {
  if (navigator.clipboard && window.isSecureContext) {
    try {
      await navigator.clipboard.writeText(text);
      return true;
    } catch {
      // Continue downgrade option
    }
  }
  try {
    const textarea = document.createElement("textarea");
    textarea.value = text;
    textarea.style.position = "fixed";
    textarea.style.left = "-9999px";
    textarea.style.top = "0";
    document.body.appendChild(textarea);
    textarea.focus();
    textarea.select();
    const ok = document.execCommand("copy");
    document.body.removeChild(textarea);
    return ok;
  } catch {
    return false;
  }
}

export const getInitials = (str: string): string => {
  if (typeof str !== "string" || !str.trim()) return "?";

  return (
    str
      .trim()
      .split(/\s+/)
      .filter(Boolean)
      .map((word) => word[0])
      .join("")
      .toUpperCase() || "?"
  );
};

export function formatCurrency(
  amount: number,
  opts?: {
    currency?: string;
    locale?: string;
    minimumFractionDigits?: number;
    maximumFractionDigits?: number;
    noDecimals?: boolean;
  },
) {
  const { currency = "USD", locale = "en-US", minimumFractionDigits, maximumFractionDigits, noDecimals } = opts ?? {};

  const formatOptions: Intl.NumberFormatOptions = {
    style: "currency",
    currency,
    minimumFractionDigits: noDecimals ? 0 : minimumFractionDigits,
    maximumFractionDigits: noDecimals ? 0 : maximumFractionDigits,
  };

  return new Intl.NumberFormat(locale, formatOptions).format(amount);
}
