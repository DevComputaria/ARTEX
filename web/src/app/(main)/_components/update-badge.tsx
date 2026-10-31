"use client";

import * as React from "react";

import Link from "next/link";

import { ArrowUpCircleIcon } from "lucide-react";

import { api } from "@/lib/api";

/**
 * The "New Version" message in the top bar: checks once the page is loaded, and updates are highlighted next to the version number,
 * Click the "Versions and Updates" tab to go directly to the system configuration page.
 *
 * The backend has a 30-minute cache of GitHub queries, so it's safe to check once every time you mount.
 * 60 times/hour/IP for unauthenticated GitHub APIs, and a few more tab pages if there is no cache.
 * The quotas are then exhausted and the actual renewal is impossible.
 *
 * The query failures are always silent: the top bar is not where errors are reported, and the user can see the reason by going to the settings page at the point "Check for updates".
 */
export function UpdateBadge() {
  const [latest, setLatest] = React.useState("");

  React.useEffect(() => {
    let alive = true;
    api
      .checkUpdate()
      .then((r) => {
        // has_update already contains the "version number comparable" judgment, and development
        // builds will not light this prompt.
        if (alive && r.has_update && r.latest) setLatest(r.latest.replace(/^v(?=\d)/, ""));
      })
      .catch(() => {
        // Silence: No network / GitHub limits should not be played in the top bar error.
      });
    return () => {
      alive = false;
    };
  }, []);

  if (!latest) return null;

  return (
    <Link
      href="/system/settings"
      title={`发现新版本 ${latest}，点击前往更新`}
      className="inline-flex items-center gap-1.5 rounded-full bg-primary px-2.5 py-1 font-medium text-primary-foreground text-xs transition-opacity hover:opacity-90"
    >
      {/* Breath points: The top bar is full of elements, plain text is easy to overlook, animation makes it visible at a glance. */}
      <span className="relative flex size-1.5">
        <span className="absolute inline-flex size-full animate-ping rounded-full bg-primary-foreground opacity-75" />
        <span className="relative inline-flex size-1.5 rounded-full bg-primary-foreground" />
      </span>
      <ArrowUpCircleIcon className="size-3.5" />
      <span className="hidden sm:inline">新版本 {latest}</span>
      <span className="sm:hidden">新版本</span>
    </Link>
  );
}
