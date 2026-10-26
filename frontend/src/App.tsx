import { AppShell, Group, Title } from "@mantine/core";
import { IconCloud } from "@tabler/icons-react";
import { Navigate, Route, Routes } from "react-router-dom";
import BackendStatus from "./components/BackendStatus";
import ErrorBoundary from "./components/ErrorBoundary";
import HeaderNav from "./components/HeaderNav";
import HeaderSearch from "./components/HeaderSearch";
import MobileNav from "./components/MobileNav";
import RequireAuth from "./components/RequireAuth";
import UploadPanel from "./components/UploadPanel";
import UserMenu from "./components/UserMenu";
import { t } from "./i18n";
import { useIsPhone } from "./media";
import AdminPage from "./pages/AdminPage";
import DeviceAuthorizePage from "./pages/DeviceAuthorizePage";
import EditorPage from "./pages/EditorPage";
import FilesPage from "./pages/FilesPage";
import LoginPage from "./pages/LoginPage";
import NotFoundPage from "./pages/NotFoundPage";
import PublicLinkPage from "./pages/PublicLinkPage";
import SearchPage from "./pages/SearchPage";
import SettingsPage from "./pages/SettingsPage";
import SharedPage from "./pages/SharedPage";
import TrashPage from "./pages/TrashPage";
import { UploadsProvider } from "./uploads";

// Shell and routes only; the router itself is provided by main.tsx (and by
// MemoryRouter in tests) so App can be rendered at any URL. The editor gets
// the whole window: it has a title and a close button of its own.
export default function App() {
  return (
    <UploadsProvider>
      <Routes>
        <Route
          path="/edit/:id"
          element={
            <ErrorBoundary>
              <RequireAuth>
                <EditorPage />
              </RequireAuth>
            </ErrorBoundary>
          }
        />
        <Route path="*" element={<Shell />} />
      </Routes>
    </UploadsProvider>
  );
}

function Shell() {
  const phone = useIsPhone();
  return (
    <AppShell header={{ height: 56 }} padding="md">
      <AppShell.Header>
        {phone ? (
          // No room for the sections in a row: they're behind the burger.
          <Group h="100%" px="md" justify="space-between" wrap="nowrap">
            <Group gap="xs" wrap="nowrap">
              <IconCloud size={28} stroke={1.5} aria-hidden />
              <Title order={3}>{t("app.title")}</Title>
            </Group>
            <Group gap="sm" wrap="nowrap">
              <HeaderSearch />
              <MobileNav />
            </Group>
          </Group>
        ) : (
          <Group h="100%" px="md" justify="space-between" wrap="nowrap">
            {/* Shrinks first on a narrow screen, so search and the user menu
                stay on it while the sections scroll sideways. */}
            <Group gap="lg" wrap="nowrap" miw={0}>
              <Group gap="xs" wrap="nowrap">
                <IconCloud size={28} stroke={1.5} aria-hidden />
                <Title order={3} visibleFrom="xs">
                  {t("app.title")}
                </Title>
              </Group>
              <HeaderNav />
            </Group>
            <Group gap="md" wrap="nowrap">
              <HeaderSearch />
              <BackendStatus />
              <UserMenu />
            </Group>
          </Group>
        )}
      </AppShell.Header>
      <AppShell.Main>
        <ErrorBoundary>
          <Routes>
            <Route path="/" element={<Navigate to="/files" replace />} />
            <Route path="/login" element={<LoginPage />} />
            <Route
              path="/files/:id?"
              element={
                <RequireAuth>
                  <FilesPage />
                </RequireAuth>
              }
            />
            <Route
              path="/shared"
              element={
                <RequireAuth>
                  <SharedPage />
                </RequireAuth>
              }
            />
            <Route
              path="/search"
              element={
                <RequireAuth>
                  <SearchPage />
                </RequireAuth>
              }
            />
            <Route
              path="/trash"
              element={
                <RequireAuth>
                  <TrashPage />
                </RequireAuth>
              }
            />
            <Route
              path="/admin"
              element={
                <RequireAuth>
                  <AdminPage />
                </RequireAuth>
              }
            />
            <Route
              path="/settings"
              element={
                <RequireAuth>
                  <SettingsPage />
                </RequireAuth>
              }
            />
            {/* The desktop app's sign-in opens this in the browser. */}
            <Route
              path="/devices/authorize"
              element={
                <RequireAuth>
                  <DeviceAuthorizePage />
                </RequireAuth>
              }
            />
            {/* No account needed: the token in the address is the key. */}
            <Route path="/s/:token/:id?" element={<PublicLinkPage />} />
            <Route path="*" element={<NotFoundPage />} />
          </Routes>
        </ErrorBoundary>
      </AppShell.Main>
      <UploadPanel />
    </AppShell>
  );
}
