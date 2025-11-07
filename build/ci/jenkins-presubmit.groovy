// This is a 'Jenkinsfile'-style declarative 'Pipeline' definition. It is
// executed by Jenkins for presubmit checks, ie. checks that run against an
// open Gerrit change request.

pipeline {
    agent none
    options {
        disableConcurrentBuilds(abortPrevious: true)
        parallelsAlwaysFailFast()
    }
    stages {
        stage('Build - Toolchain Bundle') {
            agent {
                node {
                    label ""
                    customWorkspace '/home/ci/monogon'
                }
            }
            steps {
                gerritCheck checks: ['jenkins:build_toolchain_bundle': 'RUNNING'], message: "Running on ${env.NODE_NAME}"
                echo "Gerrit change: ${GERRIT_CHANGE_URL}"
                sh "git clean -fdx -e '/bazel-*'"

                sh "JENKINS_NODE_COOKIE=dontKillMe nix-build build/toolchain/toolchain-bundle/default.nix"
            }
            post {
                success {
                    gerritCheck checks: ['jenkins:build_toolchain_bundle': 'SUCCESSFUL']
                }
                unsuccessful {
                    gerritCheck checks: ['jenkins:build_toolchain_bundle': 'FAILED']
                }
            }
        }
        stage('Gazelle') {
            agent {
                node {
                    label ""
                    customWorkspace '/home/ci/monogon'
                }
            }
            steps {
                gerritCheck checks: ['jenkins:gazelle': 'RUNNING'], message: "Running on ${env.NODE_NAME}"
                echo "Gerrit change: ${GERRIT_CHANGE_URL}"
                sh "git clean -fdx -e '/bazel-*'"
                sh "JENKINS_NODE_COOKIE=dontKillMe tools/bazel --bazelrc=.bazelrc.ci mod tidy --lockfile_mode=update"
                sh "JENKINS_NODE_COOKIE=dontKillMe tools/bazel --bazelrc=.bazelrc.ci run //:tidy"
            }
            post {
                always {
                    script {
                        def diff = sh script: "git status --porcelain", returnStdout: true
                        if (diff.trim() != "") {
                                    sh "git diff HEAD"
                            error """
                                Unclean working directory after running gazelle.
                                Please run:

                                \$ bazel mod tidy --lockfile_mode=update
                                \$ bazel run //:tidy

                                In your git checkout and amend the resulting diff to this changelist.
                            """
                        }
                    }
                }
                success {
                    gerritCheck checks: ['jenkins:gazelle': 'SUCCESSFUL']
                }
                unsuccessful {
                    gerritCheck checks: ['jenkins:gazelle': 'FAILED']
                }
            }
        }
        stage('Parallel') {
            parallel {
                stage('Test - Default') {
                    agent {
                        node {
                            label ""
                            customWorkspace '/home/ci/monogon'
                        }
                    }
                    steps {
                        gerritCheck checks: ['jenkins:test_default': 'RUNNING'], message: "Running on ${env.NODE_NAME}"
                        echo "Gerrit change: ${GERRIT_CHANGE_URL}"
                        sh "git clean -fdx -e '/bazel-*'"

                        sh "JENKINS_NODE_COOKIE=dontKillMe tools/bazel --bazelrc=.bazelrc.ci test //..."
                    }
                    post {
                        success {
                            gerritCheck checks: ['jenkins:test_default': 'SUCCESSFUL']
                        }
                        unsuccessful {
                            gerritCheck checks: ['jenkins:test_default': 'FAILED']
                        }
                    }
                }
                stage('Test - Debug') {
                    agent {
                        node {
                            label ""
                            customWorkspace '/home/ci/monogon'
                        }
                    }
                    steps {
                        gerritCheck checks: ['jenkins:test_debug': 'RUNNING'], message: "Running on ${env.NODE_NAME}"
                        echo "Gerrit change: ${GERRIT_CHANGE_URL}"
                        sh "git clean -fdx -e '/bazel-*'"

                        sh "JENKINS_NODE_COOKIE=dontKillMe tools/bazel --bazelrc=.bazelrc.ci test --config dbg //..."
                    }
                    post {
                        success {
                            gerritCheck checks: ['jenkins:test_debug': 'SUCCESSFUL']
                        }
                        unsuccessful {
                            gerritCheck checks: ['jenkins:test_debug': 'FAILED']
                        }
                    }
                }
                stage('Test - Race') {
                    agent {
                        node {
                            label ""
                            customWorkspace '/home/ci/monogon'
                        }
                    }
                    steps {
                        gerritCheck checks: ['jenkins:test_race': 'RUNNING'], message: "Running on ${env.NODE_NAME}"
                        echo "Gerrit change: ${GERRIT_CHANGE_URL}"
                        sh "git clean -fdx -e '/bazel-*'"

                        sh "JENKINS_NODE_COOKIE=dontKillMe tools/bazel --bazelrc=.bazelrc.ci test --config race //..."
                    }
                    post {
                        success {
                            gerritCheck checks: ['jenkins:test_race': 'SUCCESSFUL']
                        }
                        unsuccessful {
                            gerritCheck checks: ['jenkins:test_race': 'FAILED']
                        }
                    }
                }
            }
        }
    }
    post {
        success {
            gerritReview labels: [Verified: 1]
        }
        unsuccessful {
            gerritReview labels: [Verified: -1]
        }
    }
}