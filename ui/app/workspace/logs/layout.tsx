import { NoPermissionView } from "@/components/noPermissionView";
import { RbacOperation, RbacResource, useRbac } from "@enterprise/lib";
import { createFileRoute } from "@tanstack/react-router";
import LogsPage from "./page";

function RouteComponent() {
	const hasViewLogsAccess = useRbac(RbacResource.Logs, RbacOperation.View);
	if (!hasViewLogsAccess) {
		return <NoPermissionView entity="logs" />;
	}
	return (
		<div className="flex h-full flex-col">
			<LogsPage />
		</div>
	);
}

export const Route = createFileRoute("/workspace/logs")({
	component: RouteComponent,
});