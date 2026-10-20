export const siteConfig = {
  name: "ConOps",
  description: "GitOps for Docker Compose. Like Argo CD, but without Kubernetes.",
  links: {
    github: "https://github.com/valthaide/conops-private",
  },
  hero: {
    title: "GitOps for Docker Compose",
    subtitle: "Argo CD style reconciliation. No Kubernetes required.",
    installCommand: `docker compose up -d --build`
  }
}
