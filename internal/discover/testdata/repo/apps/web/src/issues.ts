import { useShape } from "@electric-sql/react";

export function useIssues() {
  return useShape({ url: `${ELECTRIC_URL}/v1/shape`, params: { table: "issues" } });
}
