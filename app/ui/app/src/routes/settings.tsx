import { AppSidebar } from "@/components/AppSidebar";
import { SidebarLayout } from "@/components/layout/layout";
import { Button } from "@/components/ui/button";
import { createFileRoute, useRouter } from "@tanstack/react-router";
import Settings from "@/components/Settings";

export const Route = createFileRoute("/settings")({
  component: SettingsRoute,
});

function SettingsRoute() {
  const router = useRouter();

  return (
    <SidebarLayout
      title="Settings"
      sidebar={<AppSidebar current="settings" />}
    >
      <Button
        outline
        className="self-start ml-4"
        onClick={() => router.history.back()}
      >
        Back
      </Button>

      <Settings />
    </SidebarLayout>
  );
}