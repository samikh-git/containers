import type { ReactNode } from "react";
import { Text } from "@cloudflare/kumo";

/** Section eyebrow — Text forbids className, so spacing lives on the wrapper. */
export function SectionTitle({ children }: { children: ReactNode }) {
  return (
    <div className="mb-3 uppercase tracking-wider">
      <Text variant="secondary" size="sm">
        {children}
      </Text>
    </div>
  );
}
