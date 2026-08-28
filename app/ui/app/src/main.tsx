import { StrictMode } from "react";
import ReactDOM from "react-dom/client";
import { RouterProvider, createRouter } from "@tanstack/react-router";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { routeTree } from "./routeTree.gen";
import { fetchUser } from "./api";
import { StreamingProvider } from "./contexts/StreamingContext";

type Appearance = "system" | "light" | "dark";

const APPEARANCE_KEY = "ollama-appearance";

function applyAppearance(appearance: Appearance) {
  const prefersDark = window.matchMedia(
    "(prefers-color-scheme: dark)",
  ).matches;

  const useDark =
    appearance === "dark" || (appearance === "system" && prefersDark);

  document.documentElement.classList.toggle("dark", useDark);
}

const savedAppearance =
  (window.localStorage.getItem(APPEARANCE_KEY) as Appearance | null) ??
  "system";

applyAppearance(savedAppearance);

const queryClient = new QueryClient({
  defaultOptions: {
    mutations: {
      networkMode: "always", // Run mutations regardless of network state
    },
    queries: {
      networkMode: "always", // Allow queries even when offline (local server)
    },
  },
});

fetchUser().then((userData) => {
  if (userData) {
    queryClient.setQueryData(["user"], userData);
  }
});

const router = createRouter({
  routeTree,
  context: { queryClient },
});

// Register the router instance for type safety
declare module "@tanstack/react-router" {
  interface Register {
    router: typeof router;
  }
}

const rootElement = document.getElementById("root")!;
if (!rootElement.innerHTML) {
  const root = ReactDOM.createRoot(rootElement);
  root.render(
    <StrictMode>
      <QueryClientProvider client={queryClient}>
        <StreamingProvider>
          <RouterProvider router={router} />
        </StreamingProvider>
      </QueryClientProvider>
    </StrictMode>,
  );
}